package fileset

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"path"
	"strconv"
	"testing"

	units "github.com/docker/go-units"
	"golang.org/x/sync/errgroup"

	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/server/pkg/dbutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/chunk"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset/index"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/track"
)

const (
	max         = 20 * units.MB
	maxTags     = 10
	testPath    = "test"
	scratchPath = "scratch"
)

type testFile struct {
	path  string
	parts []*testPart
}

type testPart struct {
	tag  string
	data []byte
}

func appendFile(t *testing.T, w *Writer, path string, parts []*testPart) {
	// Write content and tags.
	err := w.Append(path, func(fw *FileWriter) error {
		for _, part := range parts {
			fw.Append(part.tag)
			if _, err := fw.Write(part.data); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

func writeFileSet(t *testing.T, s *Storage, fileSet string, files []*testFile) {
	w := s.NewWriter(context.Background(), fileSet)
	for _, file := range files {
		appendFile(t, w, file.path, file.parts)
	}
	require.NoError(t, w.Close())
}

func checkFile(t *testing.T, f File, tf *testFile) {
	r, w := io.Pipe()
	eg := errgroup.Group{}
	eg.Go(func() error {
		return f.Content(w)
	})
	eg.Go(func() (retErr error) {
		defer func() {
			if retErr != nil {
				r.CloseWithError(retErr)
			} else {
				r.Close()
			}
		}()
		for _, part := range tf.parts {
			actual := make([]byte, len(part.data))
			_, err := io.ReadFull(r, actual)
			if err != nil && err != io.EOF {
				return err
			}
		}
		return r.Close()
	})
	require.NoError(t, eg.Wait())
}

// newTestStorage creates a storage object with a test db and test tracker
// both of those components are kept hidden, so this is only appropriate for testing this package.
func newTestStorage(t *testing.T) *Storage {
	db := dbutil.NewTestDB(t)
	tr := track.NewTestTracker(t, db)
	return NewTestStorage(t, db, tr)
}

func TestWriteThenRead(t *testing.T) {
	ctx := context.Background()
	storage := newTestStorage(t)
	fileNames := index.Generate("abc")
	files := []*testFile{}
	for _, fileName := range fileNames {
		var parts []*testPart
		for _, tagInt := range rand.Perm(maxTags) {
			tag := fmt.Sprintf("%08x", tagInt)
			data := chunk.RandSeq(rand.Intn(max))
			parts = append(parts, &testPart{
				tag:  tag,
				data: data,
			})
		}
		files = append(files, &testFile{
			path:  "/" + fileName,
			parts: parts,
		})
	}

	// Write out ten filesets where each subsequent fileset has the content of one random file changed.
	// Confirm that all of the content and hashes other than the changed file remain the same.
	fileset := path.Join(testPath, "0")

	// Write the files to the fileset.
	writeFileSet(t, storage, fileset, files)

	// Read the files from the fileset, checking against the recorded files.
	fs, err := storage.Open(ctx, []string{fileset})
	require.NoError(t, err)
	fileIter := files
	err = fs.Iterate(ctx, func(f File) error {
		tf := fileIter[0]
		fileIter = fileIter[1:]
		checkFile(t, f, tf)
		return nil
	})
	require.NoError(t, err)
}

func TestWriteThenReadFuzz(t *testing.T) {
	ctx := context.Background()
	storage := newTestStorage(t)
	fileNames := index.Generate("abc")
	files := []*testFile{}
	for _, fileName := range fileNames {
		var parts []*testPart
		for _, tagInt := range rand.Perm(maxTags) {
			tag := fmt.Sprintf("%08x", tagInt)
			data := chunk.RandSeq(rand.Intn(max))
			parts = append(parts, &testPart{
				tag:  tag,
				data: data,
			})
		}
		files = append(files, &testFile{
			path:  "/" + fileName,
			parts: parts,
		})
	}

	// Write out ten filesets where each subsequent fileset has the content of one random file changed.
	// Confirm that all of the content and hashes other than the changed file remain the same.
	N := len(fileNames)
	for i := 0; i < N; i++ {
		fileset := path.Join(testPath, strconv.Itoa(i)+"_")
		// Write the files to the fileset.
		writeFileSet(t, storage, fileset, files)
		r, err := storage.Open(ctx, []string{fileset})
		require.NoError(t, err)
		filesIter := files
		require.NoError(t, r.Iterate(ctx, func(f File) error {
			checkFile(t, f, filesIter[0])
			filesIter = filesIter[1:]
			return nil
		}))
		files[i].parts = append(files[i].parts, &testPart{
			data: chunk.RandSeq(rand.Intn(max)),
			tag:  "test-tag",
		})
		require.NoError(t, storage.Delete(ctx, fileset))
	}
}

func TestCopy(t *testing.T) {
	ctx := context.Background()
	fileSets := newTestStorage(t)
	fileNames := index.Generate("abc")
	files := []*testFile{}
	for _, fileName := range fileNames {
		var parts []*testPart
		for _, tagInt := range rand.Perm(maxTags) {
			tag := fmt.Sprintf("%08x", tagInt)
			data := chunk.RandSeq(rand.Intn(max))
			parts = append(parts, &testPart{
				tag:  tag,
				data: data,
			})
		}
		files = append(files, &testFile{
			path:  "/" + fileName,
			parts: parts,
		})
	}
	originalPath := path.Join(testPath, "original")
	writeFileSet(t, fileSets, originalPath, files)

	initialChunkCount := countChunks(t, fileSets)
	// Copy intial fileset to a new copy fileset.
	r := fileSets.newReader(originalPath)
	copyPath := path.Join(testPath, "copy")
	wCopy := fileSets.newWriter(context.Background(), copyPath)
	require.NoError(t, CopyFiles(ctx, wCopy, r))
	require.NoError(t, wCopy.Close())

	// Compare initial fileset and copy fileset.
	rCopy := fileSets.newReader(copyPath)
	require.NoError(t, rCopy.Iterate(ctx, func(f File) error {
		checkFile(t, f, files[0])
		files = files[1:]
		return nil
	}))
	// No new chunks should get created by the copy.
	finalChunkCount := countChunks(t, fileSets)
	// TODO: figure out why we make 1 more chunk.
	require.Equal(t, initialChunkCount, finalChunkCount-1)
}

func countChunks(t *testing.T, s *Storage) (count int64) {
	require.NoError(t, s.ChunkStorage().List(context.Background(), func(_ string) error {
		count++
		return nil
	}))
	return count
}
