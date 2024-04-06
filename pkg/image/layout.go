package image

import (
	"errors"
	"fmt"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/tomekjarosik/geranos/pkg/image/duplicator"
	"github.com/tomekjarosik/geranos/pkg/image/segmentlayer"
	"github.com/tomekjarosik/geranos/pkg/image/sparsefile"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const LocalManifestFilename = ".oci.manifest.json"
const ConfigMediaType = types.MediaType("application/online.jarosik.tomasz.v1.config+json")
const FilenameAnnotationKey = "filename"
const RangeAnnotationKey = "range"

type LayoutMapper struct {
	rootDir           string
	sketchConstructor SketchConstructor

	opts  *options
	stats Statistics
}

type Layout struct {
	Filenames []string
	Digests   map[string][]v1.Hash
	Sizes     []int64
}

func NewLayoutMapper(rootDir string, opt ...Option) *LayoutMapper {
	return &LayoutMapper{
		rootDir:           rootDir,
		sketchConstructor: NewSketchConstructor(rootDir),
		opts:              makeOptions(opt...),
	}
}

func (lm *LayoutMapper) refToDir(ref name.Reference) string {
	return filepath.Join(lm.rootDir, ref.String())
}

func (lm *LayoutMapper) contentMatches(destinationDir string, segment *fileSegmentRecipe) bool {
	fname := filepath.Join(destinationDir, segment.Filename)
	f, err := os.OpenFile(fname, os.O_RDONLY, 0666)
	if err != nil {
		return false
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			lm.opts.printf("error while closing file %v, got %v", segment.Filename, err)
		}
	}(f)
	l, err := segmentlayer.FromFile(fname, segmentlayer.WithRange(segment.Start, segment.Stop))
	if err != nil {
		return false
	}
	d, err := l.Digest()
	lm.stats.Add(&Statistics{
		BytesReadCount: segment.Length(),
	})
	if err != nil {
		return false
	}
	if d == segment.Digest {
		return true
	}
	return false
}

func (lm *LayoutMapper) writeLayer(destinationDir string, segment *fileSegmentRecipe, layer v1.Layer) (written int64, skipped int64, err error) {
	if layer == nil {
		return 0, 0, errors.New("nil layer provided")
	}
	f, err := os.OpenFile(filepath.Join(destinationDir, segment.Filename), os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return 0, 0, err
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			lm.opts.printf("error while closing file %v, got %v", segment.Filename, err)
		}
	}(f)

	_, err = f.Seek(segment.Start, io.SeekStart)
	if err != nil {
		return 0, 0, err
	}
	rc, err := layer.Uncompressed()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to access uncompressed layer: %w", err)
	}
	defer rc.Close()

	return sparsefile.Copy(f, rc)
}

func (lm *LayoutMapper) Write(img v1.Image, ref name.Reference) error {
	recipes, err := createFileRecipesFromImage(img)
	if err != nil {
		return err
	}
	destinationDir := lm.refToDir(ref)
	err = os.MkdirAll(destinationDir, 0o777)
	if err != nil {
		return err
	}
	scStats, err := lm.sketchConstructor.Construct(destinationDir, recipes)
	if err != nil {
		// TODO: ensure we don't delete anything useful _ = os.RemoveAll(destinationDir)
		return err
	}
	lm.stats.Add(&scStats)

	type Job struct {
		Recipe fileSegmentRecipe
		Layer  v1.Layer
	}
	type JobResult struct {
		Job Job
		err error
	}
	var wg sync.WaitGroup
	workersCount := min(lm.opts.workersCount, runtime.NumCPU())
	jobs := make(chan Job, workersCount)
	results := make(chan JobResult, workersCount)

	for w := 0; w < workersCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				var jobErr error
				for i := 0; i < lm.opts.networkFailureRetryCount; i++ {
					if lm.contentMatches(destinationDir, &job.Recipe) {
						break
					}
					written, skipped, err := lm.writeLayer(destinationDir, &job.Recipe, job.Layer)
					lm.opts.printf("written=%d, skipped=%d\n", written, skipped)
					lm.stats.Add(&Statistics{
						BytesWrittenCount: written,
						BytesSkippedCount: skipped,
					})
					// TODO: Check if written+skipped == (stop - start + 1)
					if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
						continue
					}
					jobErr = err
					break
				}
				results <- JobResult{Job: job, err: jobErr}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	go func() {
		for _, r := range recipes {
			for _, seg := range r.Segments {
				l, err := img.LayerByDigest(seg.Digest)
				if err != nil {
					lm.opts.printf("invalid seg.Digest")
					l = nil
				}
				jobs <- Job{Recipe: seg, Layer: l}
			}
		}
		close(jobs)
	}()
	for res := range results {
		if res.err != nil {
			return fmt.Errorf("failed writing to file '%v' at offset '%v': %w", res.Job.Recipe.Filename, res.Job.Recipe.Start, err)
		}
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destinationDir, LocalManifestFilename), rawManifest, 0o777)
}

func (lm *LayoutMapper) splitToLayers(fullpath string, chunkSize int64, workersCount int) ([]*segmentlayer.Layer, error) {
	f, err := os.Stat(fullpath)
	if err != nil {
		return nil, err
	}
	if f.Size() < chunkSize {
		l, err := segmentlayer.FromFile(fullpath)
		if err != nil {
			return nil, err
		}
		return []*segmentlayer.Layer{l}, nil
	}

	type layerResult struct {
		layer *segmentlayer.Layer
		err   error
	}

	maxIdx := f.Size() - 1
	jobs := make(chan int64, workersCount)
	results := make(chan layerResult, workersCount)

	var wg sync.WaitGroup
	for w := 0; w < workersCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for start := range jobs {
				stop := start + chunkSize - 1
				if stop > maxIdx {
					stop = maxIdx
				}
				l, err := segmentlayer.FromFile(fullpath, segmentlayer.WithRange(start, stop))
				results <- layerResult{
					layer: l,
					err:   err,
				}
				if err == nil {
					// Precompute hashes concurrently
					_, _ = l.DiffID()
					_, _ = l.Digest()
					atomic.AddInt64(&lm.stats.BytesReadCount, 2*(stop-start+1))
				}
			}
		}()
	}
	go func() {
		for start := int64(0); start <= maxIdx; start += chunkSize {
			jobs <- start
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	sortedLayers := make([]layerResult, 0)
	for r := range results {
		if r.err != nil {
			return nil, r.err
		}
		sortedLayers = append(sortedLayers, r)
	}
	sort.Slice(sortedLayers, func(i, j int) bool {
		return sortedLayers[i].layer.Start() < sortedLayers[j].layer.Start()
	})
	res := make([]*segmentlayer.Layer, 0)
	for _, r := range sortedLayers {
		res = append(res, r.layer)
	}
	return res, nil
}

func (lm *LayoutMapper) Read(ref name.Reference) (v1.Image, error) {
	img := empty.Image
	img = mutate.ConfigMediaType(img, ConfigMediaType)

	dirEntries, err := os.ReadDir(filepath.Join(lm.rootDir, ref.String()))
	if err != nil {
		return nil, err
	}
	for _, entry := range dirEntries {
		if entry.IsDir() {
			lm.opts.printf("unexpected subdirectory '%v', skipping", entry.Name())
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			lm.opts.printf("skipping file '%v' because it starts with a dot", entry.Name())
			continue
		}
		layers, err := lm.splitToLayers(filepath.Join(lm.rootDir, ref.String(), entry.Name()), lm.opts.chunkSize, lm.opts.workersCount)
		if err != nil {
			return nil, err
		}
		addendums := make([]mutate.Addendum, 0)
		for _, l := range layers {
			addendums = append(addendums, mutate.Addendum{
				Layer:   l,
				History: v1.History{},
				Annotations: map[string]string{
					FilenameAnnotationKey: entry.Name(),
					RangeAnnotationKey:    fmt.Sprintf("%d-%d", l.Start(), l.Stop()),
				},
				MediaType: segmentlayer.FileSegmentMediaType,
			})
		}
		img, err = mutate.Append(img, addendums...)
		if err != nil {
			return nil, fmt.Errorf("unable to append layers to image: %w", err)
		}
	}
	diffIDs := make([]v1.Hash, 0)
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		h, err := l.DiffID()
		if err != nil {
			return nil, err
		}
		diffIDs = append(diffIDs, h)
	}

	return mutate.ConfigFile(img, &v1.ConfigFile{
		Architecture: "arm64", // TODO:
		Container:    "geranos",
		Created:      v1.Time{Time: time.Now()},
		OS:           "",
		RootFS:       v1.RootFS{Type: "layers", DiffIDs: diffIDs},
		OSVersion:    "TODO",
		Variant:      "",
	})
}

// IsDirWithOnlyFiles checks if the given path is a directory that contains only files (no subdirectories).
func IsDirWithOnlyFiles(path string) (bool, error) {
	// Check the provided path is indeed a directory
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !fileInfo.IsDir() {
		return false, nil // Not even a directory
	}

	// Read the directory contents
	contents, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}

	// Iterate over the directory contents
	for _, content := range contents {
		if content.IsDir() {
			return false, nil // Found a subdirectory, so return false
		}
	}

	return true, nil // No subdirectories found, only files
}

func (lm *LayoutMapper) Adopt(src string, ref name.Reference, failIfContainsSubdirectories bool) error {
	isFlatDir, err := IsDirWithOnlyFiles(src)
	if err != nil {
		return fmt.Errorf("unable to verify if directory is flat: %w", err)
	}
	if !isFlatDir && failIfContainsSubdirectories {
		return fmt.Errorf("directory with subdirectories are not supported")
	}
	if failIfContainsSubdirectories {
		lm.opts.printf("warning: subdirectories will be ignored")
	}
	return duplicator.CloneDirectory(src, lm.refToDir(ref), false)
}

type Properties struct {
	Ref       name.Reference
	DiskUsage string
	Size      int64
}

func directorySize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err
}

func (lm *LayoutMapper) ContainsManifest(ref name.Reference) bool {
	return true
}

func (lm *LayoutMapper) List() ([]Properties, error) {
	res := make([]Properties, 0)
	err := filepath.WalkDir(lm.rootDir, func(path string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			return nil
		}
		processedPath := strings.TrimPrefix(path, lm.rootDir)
		processedPath = strings.Trim(processedPath, "/")
		ref, err := name.ParseReference(processedPath, name.StrictValidation)
		if err != nil {
			return nil
		}
		if !lm.ContainsManifest(ref) {
			return nil
		}
		dirSize, err := directorySize(path)
		if err != nil {
			dirSize = -1
		}
		diskUsage, err := directoryDiskUsage(path)
		if err != nil {
			return err
		}
		res = append(res, Properties{
			Ref:       ref,
			DiskUsage: diskUsage,
			Size:      dirSize,
		})
		return nil
	})
	return res, err
}

func (lm *LayoutMapper) Clone(src name.Reference, dst name.Reference) error {
	return duplicator.CloneDirectory(lm.refToDir(src), lm.refToDir(dst), true)
}

func (lm *LayoutMapper) Remove(src name.Reference) error {
	ref, err := name.ParseReference(src.String(), name.StrictValidation)
	if err != nil {
		return fmt.Errorf("unable to valid reference: %w", err)
	}
	return os.RemoveAll(filepath.Join(lm.rootDir, lm.refToDir(ref)))
}

func (lm *LayoutMapper) Stats() Statistics {
	return lm.stats
}
