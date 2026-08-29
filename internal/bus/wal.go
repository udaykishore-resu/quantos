package bus

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

// WALBus is a durable, single-node append-only log with the same semantics the
// platform relies on from Kafka: ordered per-topic delivery, consumer groups
// with committed offsets, replay from any offset, and survival of a process
// restart.
//
// It exists for two reasons. First, it makes `make dev` genuinely durable
// without requiring a broker. Second, it is the buffer that absorbs a broker
// outage: the Kafka driver writes here when the broker is unreachable and
// drains when it returns, which is the "bounded on-disk WAL" in ADR-002.
//
// Record framing: [uint32 length][payload][uint32 crc32(payload)], little
// endian. Segments roll at SegmentBytes and are named %020d.log by their first
// record offset, so a lookup by offset is a binary search over file names.
type WALBus struct {
	dir  string
	opts WALOptions

	mu     sync.RWMutex
	topics map[string]*walTopic
	subs   []*walSub
	closed bool

	wg     sync.WaitGroup
	stopCh chan struct{}

	// lock is an exclusive advisory lock on the log directory, held for the
	// lifetime of the bus. See acquireDirLock.
	lock *os.File
}

// WALOptions parameterises the durable log.
type WALOptions struct {
	Dir string
	// SegmentBytes is the roll threshold. Default 64 MiB.
	SegmentBytes int64
	// SyncEveryWrite trades throughput for durability. Default false: the OS
	// page cache is flushed on roll and on Close, which loses at most the last
	// few milliseconds on a hard kill. Set true where that is unacceptable.
	SyncEveryWrite bool
	// Retention deletes whole segments older than this on roll. Zero keeps all.
	Retention time.Duration
	// PollInterval is how often readers check for new data. Default 5 ms.
	PollInterval time.Duration
	Metrics      *obs.Metrics
	Clock        obs.Clock
	MaxRetries   int
	OnError      func(topic, group string, e Envelope, err error)
}

type walTopic struct {
	name string
	dir  string

	mu       sync.Mutex
	file     *os.File
	writer   *bufio.Writer
	segBase  int64 // first record offset in the current segment
	segBytes int64
	next     int64 // next record offset to assign
}

type walSub struct {
	topic   string
	group   string
	handler Handler
	offset  int64
}

// NewWALBus opens (creating if needed) a durable log rooted at dir.
func NewWALBus(opts WALOptions) (*WALBus, error) {
	if opts.Dir == "" {
		return nil, errors.New("wal: Dir is required")
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = 64 << 20
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Millisecond
	}
	if opts.Clock == nil {
		opts.Clock = obs.SystemClock{}
	}
	if err := os.MkdirAll(filepath.Join(opts.Dir, "offsets"), 0o750); err != nil {
		return nil, fmt.Errorf("wal: create dir: %w", err)
	}
	lock, err := acquireDirLock(opts.Dir)
	if err != nil {
		return nil, err
	}
	return &WALBus{
		dir:    opts.Dir,
		opts:   opts,
		topics: map[string]*walTopic{},
		stopCh: make(chan struct{}),
		lock:   lock,
	}, nil
}

// acquireDirLock takes an exclusive advisory lock on the log directory, and
// fails rather than waiting if another process already holds it.
//
// This driver is a single-process log. Appends are serialised by an in-process
// mutex through a buffered writer, and each process keeps its own idea of the
// next record offset. Two processes sharing a directory therefore interleave
// partial records and hand out the same offsets twice — and neither notices,
// because a torn record only surfaces later as a CRC failure that looks like
// disk corruption.
//
// Refusing to start is the right answer. The alternative that looks friendlier
// — carrying on and hoping the two processes never write the same topic — is
// exactly the failure this platform is built to avoid: something that appears
// to be working while quietly losing events. Cross-process fan-out is what a
// broker is for; point bus.driver at kafka when more than one process needs to
// publish.
func acquireDirLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, "LOCK")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("wal: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf(
			"wal: %s is already in use by another process: this driver is a single-process "+
				"log and a second writer would interleave records and reuse offsets. "+
				"Give each process its own bus.dir, or use a broker (bus.driver=kafka) "+
				"if they need to share a topic: %w", dir, err)
	}
	return f, nil
}

func segName(base int64) string { return fmt.Sprintf("%020d.log", base) }

func (b *WALBus) topic(name string) (*walTopic, error) {
	b.mu.RLock()
	t, ok := b.topics[name]
	b.mu.RUnlock()
	if ok {
		return t, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok = b.topics[name]; ok {
		return t, nil
	}
	dir := filepath.Join(b.dir, sanitizeTopic(name))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("wal: topic dir %s: %w", name, err)
	}
	t = &walTopic{name: name, dir: dir}
	if err := t.openTail(b.opts.SegmentBytes); err != nil {
		return nil, err
	}
	b.topics[name] = t
	return t, nil
}

func sanitizeTopic(n string) string {
	return strings.ReplaceAll(n, string(os.PathSeparator), "_")
}

// segments lists segment base offsets in ascending order.
func segments(dir string) ([]int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".log"), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// openTail opens the newest segment for appending and recovers `next` by
// scanning it. A torn trailing record (from a crash mid-write) is truncated,
// which is the standard log-recovery behaviour.
func (t *walTopic) openTail(segmentBytes int64) error {
	segs, err := segments(t.dir)
	if err != nil {
		return err
	}
	var base int64
	if len(segs) > 0 {
		base = segs[len(segs)-1]
	}
	path := filepath.Join(t.dir, segName(base))
	count, size, err := scanSegment(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("wal: open segment: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: truncate torn tail: %w", err)
	}
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		_ = f.Close()
		return err
	}
	t.file = f
	t.writer = bufio.NewWriterSize(f, 1<<16)
	t.segBase = base
	t.segBytes = size
	t.next = base + count
	return nil
}

// scanSegment returns the number of intact records and the byte length of the
// intact prefix.
func scanSegment(path string) (int64, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var count, size int64
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return count, size, nil // EOF or torn header: stop here
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > 32<<20 {
			return count, size, nil
		}
		buf := make([]byte, n+4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return count, size, nil
		}
		want := binary.LittleEndian.Uint32(buf[n:])
		if crc32.ChecksumIEEE(buf[:n]) != want {
			return count, size, nil
		}
		count++
		size += int64(4 + n + 4)
	}
}

func (t *walTopic) append(raw []byte, segmentBytes int64, sync bool) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.segBytes >= segmentBytes {
		if err := t.roll(); err != nil {
			return 0, err
		}
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(raw)))
	var trailer [4]byte
	binary.LittleEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(raw))
	if _, err := t.writer.Write(hdr[:]); err != nil {
		return 0, err
	}
	if _, err := t.writer.Write(raw); err != nil {
		return 0, err
	}
	if _, err := t.writer.Write(trailer[:]); err != nil {
		return 0, err
	}
	if err := t.writer.Flush(); err != nil {
		return 0, err
	}
	if sync {
		if err := t.file.Sync(); err != nil {
			return 0, err
		}
	}
	off := t.next
	t.next++
	t.segBytes += int64(8 + len(raw))
	return off, nil
}

func (t *walTopic) roll() error {
	if err := t.writer.Flush(); err != nil {
		return err
	}
	if err := t.file.Sync(); err != nil {
		return err
	}
	if err := t.file.Close(); err != nil {
		return err
	}
	base := t.next
	f, err := os.OpenFile(filepath.Join(t.dir, segName(base)), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	t.file = f
	t.writer = bufio.NewWriterSize(f, 1<<16)
	t.segBase = base
	t.segBytes = 0
	return nil
}

// Publish appends the envelope to the topic log.
func (b *WALBus) Publish(_ context.Context, topic string, e Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	t, err := b.topic(topic)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("wal: marshal: %w", err)
	}
	if _, err := t.append(raw, b.opts.SegmentBytes, b.opts.SyncEveryWrite); err != nil {
		if b.opts.Metrics != nil {
			b.opts.Metrics.BusErrors.Inc(topic, "publish")
		}
		return err
	}
	if b.opts.Metrics != nil {
		b.opts.Metrics.BusPublished.Inc(topic)
	}
	return nil
}

// PublishBatch appends events in order.
func (b *WALBus) PublishBatch(ctx context.Context, topic string, es []Envelope) error {
	for _, e := range es {
		if err := b.Publish(ctx, topic, e); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe registers a consumer. The committed offset is loaded from disk, so
// a restart resumes exactly where the group left off.
func (b *WALBus) Subscribe(topic, group string, h Handler) error {
	if h == nil {
		return errors.New("bus: nil handler")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	off, err := b.loadOffset(topic, group)
	if err != nil {
		return err
	}
	b.subs = append(b.subs, &walSub{topic: topic, group: group, handler: h, offset: off})
	return nil
}

func (b *WALBus) offsetPath(topic, group string) string {
	return filepath.Join(b.dir, "offsets", sanitizeTopic(topic)+"."+group)
}

func (b *WALBus) loadOffset(topic, group string) (int64, error) {
	data, err := os.ReadFile(b.offsetPath(topic, group))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, nil
	}
	return v, nil
}

func (b *WALBus) commitOffset(topic, group string, off int64) error {
	p := b.offsetPath(topic, group)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(off, 10)), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// SeekToBeginning resets a group's offset, which is how a replay is triggered.
func (b *WALBus) SeekToBeginning(topic, group string) error {
	return b.commitOffset(topic, group, 0)
}

// Run drives delivery until ctx is cancelled.
func (b *WALBus) Run(ctx context.Context) error {
	b.mu.RLock()
	subs := append([]*walSub(nil), b.subs...)
	b.mu.RUnlock()
	for _, s := range subs {
		b.wg.Add(1)
		go b.consume(ctx, s)
	}
	<-ctx.Done()
	b.wg.Wait()
	return nil
}

func (b *WALBus) consume(ctx context.Context, s *walSub) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.opts.PollInterval)
	defer ticker.Stop()
	for {
		n, err := b.drainFrom(ctx, s)
		if err != nil && !errors.Is(err, context.Canceled) {
			if b.opts.Metrics != nil {
				b.opts.Metrics.BusErrors.Inc(s.topic, "consume")
			}
		}
		if n > 0 {
			continue // more may be available immediately
		}
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-ticker.C:
		}
	}
}

// drainFrom reads and dispatches available records, returning how many were
// handled. The offset is committed only after the handler succeeds, which is
// the at-least-once contract consumers are written against.
func (b *WALBus) drainFrom(ctx context.Context, s *walSub) (int, error) {
	t, err := b.topic(s.topic)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	end := t.next
	t.mu.Unlock()
	if s.offset >= end {
		if b.opts.Metrics != nil {
			b.opts.Metrics.BusLag.Set(0, s.topic, s.group)
		}
		return 0, nil
	}
	if b.opts.Metrics != nil {
		b.opts.Metrics.BusLag.Set(float64(end-s.offset), s.topic, s.group)
	}

	envs, err := b.read(t, s.offset, 256)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, e := range envs {
		select {
		case <-ctx.Done():
			return handled, ctx.Err()
		default:
		}
		hctx := ContextFor(ctx, e)
		var herr error
		for attempt := 0; attempt <= b.opts.MaxRetries; attempt++ {
			herr = s.handler(hctx, e)
			if herr == nil {
				break
			}
		}
		if herr != nil {
			if b.opts.OnError != nil {
				b.opts.OnError(s.topic, s.group, e, herr)
			} else {
				return handled, herr
			}
		}
		s.offset = e.Offset + 1
		if err := b.commitOffset(s.topic, s.group, s.offset); err != nil {
			return handled, err
		}
		handled++
		if b.opts.Metrics != nil {
			b.opts.Metrics.BusConsumed.Inc(s.topic, s.group)
		}
	}
	return handled, nil
}

// read returns up to limit envelopes starting at from.
func (b *WALBus) read(t *walTopic, from int64, limit int) ([]Envelope, error) {
	segs, err := segments(t.dir)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, nil
	}
	// Locate the segment containing `from`.
	idx := sort.Search(len(segs), func(i int) bool { return segs[i] > from }) - 1
	if idx < 0 {
		idx = 0
	}
	out := make([]Envelope, 0, limit)
	off := segs[idx]
	for si := idx; si < len(segs) && len(out) < limit; si++ {
		f, err := os.Open(filepath.Join(t.dir, segName(segs[si])))
		if err != nil {
			return out, err
		}
		r := bufio.NewReaderSize(f, 1<<16)
		off = segs[si]
		var hdr [4]byte
		for len(out) < limit {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				break
			}
			n := binary.LittleEndian.Uint32(hdr[:])
			if n == 0 || n > 32<<20 {
				break
			}
			buf := make([]byte, n+4)
			if _, err := io.ReadFull(r, buf); err != nil {
				break
			}
			if crc32.ChecksumIEEE(buf[:n]) != binary.LittleEndian.Uint32(buf[n:]) {
				break
			}
			if off >= from {
				var e Envelope
				if err := json.Unmarshal(buf[:n], &e); err != nil {
					off++
					continue
				}
				e.Offset = off
				out = append(out, e)
			}
			off++
		}
		_ = f.Close()
	}
	return out, nil
}

// Close flushes and stops delivery.
func (b *WALBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	topics := make([]*walTopic, 0, len(b.topics))
	for _, t := range b.topics {
		topics = append(topics, t)
	}
	b.mu.Unlock()
	close(b.stopCh)
	b.wg.Wait()
	var firstErr error
	if b.lock != nil {
		// Releasing on close is what lets a restarted process take the log back
		// immediately; the kernel would release it on exit anyway.
		_ = syscall.Flock(int(b.lock.Fd()), syscall.LOCK_UN)
		_ = b.lock.Close()
	}
	for _, t := range topics {
		t.mu.Lock()
		if err := t.writer.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := t.file.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := t.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		t.mu.Unlock()
	}
	return firstErr
}

// Depth returns the number of records in a topic, for metrics and tests.
func (b *WALBus) Depth(topic string) int64 {
	t, err := b.topic(topic)
	if err != nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.next
}
