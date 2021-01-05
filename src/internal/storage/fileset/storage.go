package fileset

import (
	"context"
	"math"
	"strings"
	"time"

	units "github.com/docker/go-units"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/storage/chunk"
	"github.com/pachyderm/pachyderm/v2/src/internal/storage/fileset/index"
	"github.com/pachyderm/pachyderm/v2/src/internal/storage/renew"
	"github.com/pachyderm/pachyderm/v2/src/internal/storage/track"
	"golang.org/x/sync/semaphore"
)

const (
	// DefaultMemoryThreshold is the default for the memory threshold that must
	// be met before a file set part is serialized (excluding close).
	DefaultMemoryThreshold = 1024 * units.MB
	// DefaultShardThreshold is the default for the size threshold that must
	// be met before a shard is created by the shard function.
	DefaultShardThreshold = 1024 * units.MB
	// DefaultLevelFactor is the default factor that level sizes increase by in a compacted fileset
	DefaultLevelFactor = 10

	// Diff is the suffix of a path that points to the diff of the prefix.
	Diff = "diff"
	// Compacted is the suffix of a path that points to the compaction of the prefix.
	Compacted = "compacted"
	// TrackerPrefix is used for creating tracker objects for filesets
	TrackerPrefix = "fileset/"
)

var (
	// ErrNoFileSetFound is returned by the methods on Storage when a fileset does not exist
	ErrNoFileSetFound = errors.Errorf("no fileset found")
)

// Storage is the abstraction that manages fileset storage.
type Storage struct {
	tracker                      track.Tracker
	store                        Store
	chunks                       *chunk.Storage
	memThreshold, shardThreshold int64
	levelFactor                  int64
	filesetSem                   *semaphore.Weighted
}

// NewStorage creates a new Storage.
func NewStorage(store Store, tr track.Tracker, chunks *chunk.Storage, opts ...StorageOption) *Storage {
	s := &Storage{
		store:          store,
		tracker:        tr,
		chunks:         chunks,
		memThreshold:   DefaultMemoryThreshold,
		shardThreshold: DefaultShardThreshold,
		levelFactor:    DefaultLevelFactor,
		filesetSem:     semaphore.NewWeighted(math.MaxInt64),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Store returns the underlying store.
// TODO Store is just used to poke through the information about file set sizes.
// I think there might be a cleaner way to handle this through the file set interface, and changing
// the metadata we expose for a file set as a set of metadata entries.
func (s *Storage) Store() Store {
	return s.store
}

// ChunkStorage returns the underlying chunk storage instance for this storage instance.
func (s *Storage) ChunkStorage() *chunk.Storage {
	return s.chunks
}

// NewUnorderedWriter creates a new unordered file set writer.
func (s *Storage) NewUnorderedWriter(ctx context.Context, defaultTag string, opts ...UnorderedWriterOption) (*UnorderedWriter, error) {
	return newUnorderedWriter(ctx, s, s.memThreshold, defaultTag, opts...)
}

// NewWriter creates a new file set writer.
func (s *Storage) NewWriter(ctx context.Context, opts ...WriterOption) *Writer {
	return s.newWriter(ctx, opts...)
}

func (s *Storage) newWriter(ctx context.Context, opts ...WriterOption) *Writer {
	return newWriter(ctx, s, s.tracker, s.chunks, opts...)
}

// TODO: Expose some notion of read ahead (read a certain number of chunks in parallel).
// this will be necessary to speed up reading large files.
func (s *Storage) newReader(fileSet string, opts ...index.Option) *Reader {
	return newReader(s.store, s.chunks, fileSet, opts...)
}

// Open opens a file set for reading.
// TODO: It might make sense to have some of the file set transforms as functional options here.
func (s *Storage) Open(ctx context.Context, ids []ID, opts ...index.Option) (FileSet, error) {
	var fss []FileSet
	for _, id := range ids {
		md, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch x := md.Value.(type) {
		case *Metadata_Primitive:
			fss = append(fss, s.newReader(id, opts...))
		case *Metadata_Composite:
			fs, err := s.Open(ctx, x.Composite.Layers)
			if err != nil {
				return nil, err
			}
			fss = append(fss, fs)
		}
	}
	if len(fss) == 0 {
		return nil, errors.Errorf("error opening fileset: non-existent fileset: %v", fileSets)
	}
	if len(fss) == 1 {
		return fss[0], nil
	}
	return newMergeReader(s.chunks, fss), nil
}

// Compose produces a composite fileset from the filesets under ids
func (s *Storage) Compose(ctx context.Context, ids []ID, ttl time.Duration) (*ID, error) {
	c := &Composite{
		Layers: ids,
	}
	return s.newComposite(ctx, c, ttl)
}

// Clone creates a new fileset, identical to the fileset at id, but with the specified ttl.
// The ttl can be ignored by using track.NoTTL
func (s *Storage) Clone(ctx context.Context, id ID, ttl time.Duration) (*ID, error) {
	md, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	id2 := newID()
	if err := s.store.Set(ctx, id2, md); err != nil {
		return nil, err
	}
	return &id2, nil
}

// Flatten takes a list of IDs and replaces references to composite FileSets
// with references to all their layers inline.
// The returned IDs will only contain ids of Primitive FileSets
func (s *Storage) Flatten(ctx context.Context, ids []ID) ([]ID, error) {
	flattened := make([]ID, 0, len(ids))
	for _, id := range ids {
		md, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch x := md.Value.(type) {
		case *Metadata_Primitive:
			flattened = append(flattened, id)
		case *Metadata_Composite:
			ids2, err := s.Flatten(ctx, x.Composite.Layers)
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ids2...)
		}
	}
	return flattened, nil
}

// Drop allows a fileset to be deleted if it is no otherwise referenced.
func (s *Storage) Drop(ctx context.Context, id string) error {
	_, err := s.tracker.SetTTLPrefix(ctx, id, -1)
	return err
}

// SetTTL sets the time-to-live for the prefix p.
func (s *Storage) SetTTL(ctx context.Context, p string, ttl time.Duration) (time.Time, error) {
	oid := filesetObjectID(p)
	return s.tracker.SetTTLPrefix(ctx, oid, ttl)
}

// WithRenewer calls cb with a Renewer, and a context which will be canceled if the renewer is unable to renew a path.
func (s *Storage) WithRenewer(ctx context.Context, ttl time.Duration, cb func(context.Context, *renew.StringSet) error) error {
	rf := func(ctx context.Context, p string, ttl time.Duration) error {
		_, err := s.SetTTL(ctx, p, ttl)
		return err
	}
	return renew.WithStringSet(ctx, ttl, rf, cb)
}

// GC creates a track.GarbageCollector with a Deleter that can handle deleting filesets and chunks
func (s *Storage) GC(ctx context.Context) error {
	const period = 10 * time.Second
	tmpDeleter := track.NewTmpDeleter()
	chunkDeleter := s.chunks.NewDeleter()
	filesetDeleter := &deleter{
		store: s.store,
	}
	mux := track.DeleterMux(func(id string) track.Deleter {
		switch {
		case strings.HasPrefix(id, track.TmpTrackerPrefix):
			return tmpDeleter
		case strings.HasPrefix(id, chunk.TrackerPrefix):
			return chunkDeleter
		case strings.HasPrefix(id, TrackerPrefix):
			return filesetDeleter
		default:
			return nil
		}
	})
	gc := track.NewGarbageCollector(s.tracker, period, mux)
	return gc.Run(ctx)
}

func (s *Storage) newPrimitive(ctx context.Context, prim *Primitive, ttl time.Duration) (*ID, error) {
	id := newID()
	md := &Metadata{
		Value: &Metadata_Primitive{
			Primitive: prim,
		},
	}
	if err := s.store.Set(ctx, id, md); err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Storage) newComposite(ctx context.Context, comp *Composite, ttl time.Duration) (*ID, error) {
	id := newID()
	md := &Metadata{
		Value: &Metadata_Composite{
			Composite: comp,
		},
	}
	var pointsTo []string
	for _, id := range comp.Layers {
		pointsTo = append(pointsTo, filesetObjectID(id))
	}
	if err := s.tracker.CreateObject(ctx, filesetObjectID(id), pointsTo, ttl); err != nil {
		return nil, err
	}
	if err := s.store.Set(ctx, id, md); err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Storage) getPrimitive(ctx context.Context, id ID) (*Primitive, error) {
	md, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	prim := md.GetPrimitive()
	if prim == nil {
		return nil, errors.Errorf("fileset %v is not primitive", id)
	}
	return prim, nil
}

func filesetObjectID(id ID) string {
	return "fileset/" + id
}

var _ track.Deleter = &deleter{}

type deleter struct {
	store Store
}

// TODO: This needs to be implemented, temporary filesets are still in Postgres.
func (d *deleter) Delete(ctx context.Context, id string) error {
	return nil
}
