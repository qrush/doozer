package store

import (
	"container/heap"
	"container/vector"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Special values for a revision.
const (
	Missing = int64(-iota)
	Clobber
	Dir
	nop
)

// TODO revisit this when package regexp is more complete (e.g. do Unicode)
const charPat = `[a-zA-Z0-9.\-]`

var pathRe = mustBuildRe(charPat)

var Any = MustCompileGlob("/**")

var ErrTooLate = os.NewError("too late")

var (
	ErrBadMutation = os.NewError("bad mutation")
	ErrRevMismatch = os.NewError("rev mismatch")
)

type BadPathError struct {
	Path string
}

func (e *BadPathError) String() string {
	return "bad path: " + e.Path
}

func mustBuildRe(p string) *regexp.Regexp {
	return regexp.MustCompile(`^/$|^(/` + p + `+)+$`)
}

// Applies mutations sent on Ops in sequence according to field Seqn. Any
// errors that occur will be written to ErrorPath. Duplicate operations at a
// given position are sliently ignored.
type Store struct {
	Ops     chan<- Op
	Seqns   <-chan int64
	Watches <-chan int
	watchCh chan *Watch
	watches []*Watch
	todo    *vector.Vector
	state   *state
	head    int64
	log     map[int64]Event
	cleanCh chan int64
	notices []notice
	flush   chan bool
}

// Represents an operation to apply to the store at position Seqn.
//
// If Mut is Nop, no change will be made, but an event will still be sent.
type Op struct {
	Seqn int64
	Mut  string
}


// Satisfies vector.LessInterface.
func (x Op) Less(y interface{}) bool {
	return x.Seqn < y.(Op).Seqn
}


type state struct {
	ver  int64
	root node
}

type Watch struct {
	C        <-chan Event
	c        chan<- Event
	glob     *Glob
	from, to int64
	shutdown chan bool
	stopped  bool
}


func (w *Watch) isStopped() bool {
	if w.stopped {
		return true
	}

	select {
	case <-w.shutdown:
		w.stopped = true
		return true
	default:
	}

	return false
}


func (wt *Watch) Stop() {
	select {
	case wt.shutdown <- true:
	default:
	}
}

type notice struct {
	w  *Watch
	ev Event
}

// Creates a new, empty data store. Mutations will be applied in order,
// starting at number 1 (number 0 can be thought of as the creation of the
// store).
func New() *Store {
	ops := make(chan Op)
	seqns := make(chan int64)
	watches := make(chan int)

	st := &Store{
		Ops:     ops,
		Seqns:   seqns,
		Watches: watches,
		watchCh: make(chan *Watch),
		todo:    new(vector.Vector),
		watches: []*Watch{},
		state:   &state{0, emptyDir},
		log:     map[int64]Event{},
		cleanCh: make(chan int64),
		flush:   make(chan bool),
	}

	go st.process(ops, seqns, watches)
	return st
}

func split(path string) []string {
	if path == "/" {
		return []string{}
	}
	return strings.Split(path[1:], "/", -1)
}

func join(parts []string) string {
	return "/" + strings.Join(parts, "/")
}

func checkPath(k string) os.Error {
	if !pathRe.MatchString(k) {
		return &BadPathError{k}
	}
	return nil
}

// Returns a mutation that can be applied to a `Store`. The mutation will set
// the contents of the file at `path` to `body` iff `rev` is greater than
// of equal to the file's revision at the time of application, with
// one exception: if `rev` is Clobber, the file will be set unconditionally.
//
// If `path` is not valid, returns a `BadPathError`.
func EncodeSet(path, body string, rev int64) (mutation string, err os.Error) {
	if err = checkPath(path); err != nil {
		return
	}
	return strconv.Itoa64(rev) + ":" + path + "=" + body, nil
}

// Returns a mutation that can be applied to a `Store`. The mutation will cause
// the file at `path` to be deleted iff `rev` is greater than
// of equal to the file's revision at the time of application, with
// one exception: if `rev` is Clobber, the file will be deleted
// unconditionally.
//
// If `path` is not valid, returns a `BadPathError`.
func EncodeDel(path string, rev int64) (mutation string, err os.Error) {
	if err := checkPath(path); err != nil {
		return
	}
	return strconv.Itoa64(rev) + ":" + path, nil
}

// MustEncodeSet is like EncodeSet but panics if the mutation cannot be
// encoded. It simplifies safe initialization of global variables holding
// mutations.
func MustEncodeSet(path, body string, rev int64) (mutation string) {
	m, err := EncodeSet(path, body, rev)
	if err != nil {
		panic(err)
	}
	return m
}

// MustEncodeDel is like EncodeDel but panics if the mutation cannot be
// encoded. It simplifies safe initialization of global variables holding
// mutations.
func MustEncodeDel(path string, rev int64) (mutation string) {
	m, err := EncodeDel(path, rev)
	if err != nil {
		panic(err)
	}
	return m
}

func decode(mutation string) (path, v string, rev int64, keep bool, err os.Error) {
	cm := strings.Split(mutation, ":", 2)

	if len(cm) != 2 {
		err = ErrBadMutation
		return
	}

	rev, err = strconv.Atoi64(cm[0])
	if err != nil {
		return
	}

	kv := strings.Split(cm[1], "=", 2)

	if err = checkPath(kv[0]); err != nil {
		return
	}

	switch len(kv) {
	case 1:
		return kv[0], "", rev, false, nil
	case 2:
		return kv[0], kv[1], rev, true, nil
	}
	panic("unreachable")
}

func (st *Store) notify(e Event, ws []*Watch) []*Watch {
	nwatches := make([]*Watch, len(ws))

	i := 0
	for _, w := range ws {
		if w.isStopped() {
			continue
		}

		if e.Seqn >= w.to {
			continue
		}

		last := w.to - 1
		if e.Seqn != last {
			nwatches[i] = w
			i++
		}

		if e.Seqn < w.from {
			continue
		}

		if w.glob.Match(e.Path) {
			st.notices = append(st.notices, notice{w, e})
		}
	}

	return nwatches[0:i]
}

func (st *Store) closeWatches() {
	for _, w := range st.watches {
		close(w.c)
	}
}

func (st *Store) process(ops <-chan Op, seqns chan<- int64, watches chan<- int) {
	defer st.closeWatches()

	for {
		var flush bool
		ver, values := st.state.ver, st.state.root

		for len(st.notices) > 0 && st.notices[0].w.isStopped() {
			st.notices = st.notices[1:]
		}

		var nc chan<- Event
		var ne Event
		if len(st.notices) > 0 {
			nc = st.notices[0].w.c
			ne = st.notices[0].ev
		}

		// Take any incoming requests and queue them up.
		select {
		case a := <-ops:
			if closed(ops) {
				return
			}

			if a.Seqn > ver {
				heap.Push(st.todo, a)
			}
		case w := <-st.watchCh:
			n, ws := w.from, []*Watch{w}
			for ; len(ws) > 0 && n < st.head; n++ {
				ws = []*Watch{}
			}
			for ; len(ws) > 0 && n <= ver; n++ {
				ws = st.notify(st.log[n], ws)
			}

			st.watches = append(st.watches, ws...)
		case seqn := <-st.cleanCh:
			for ; st.head <= seqn; st.head++ {
				st.log[st.head] = Event{}, false
			}
		case seqns <- ver:
			// nothing to do here
		case watches <- len(st.watches):
			// nothing to do here
		case nc <- ne:
			st.notices = st.notices[1:]
		case flush = <-st.flush:
			// nothing
		}

		var ev Event
		// If we have any mutations that can be applied, do them.
		for st.todo.Len() > 0 {
			t := st.todo.At(0).(Op)
			if flush && ver < t.Seqn {
				ver = t.Seqn - 1
			}
			if t.Seqn > ver+1 {
				break
			}

			heap.Pop(st.todo)
			if t.Seqn < ver+1 {
				continue
			}

			values, ev = values.apply(t.Seqn, t.Mut)
			st.state = &state{ev.Seqn, values}
			ver = ev.Seqn
			if !flush {
				st.log[ev.Seqn] = ev
				st.watches = st.notify(ev, st.watches)
			}
		}

		// A flush just gets one final event.
		if flush {
			st.log[ev.Seqn] = ev
			st.watches = st.notify(ev, st.watches)
			st.head = ver + 1
		}
	}
}

// Returns a point-in-time snapshot of the contents of the store.
func (st *Store) Snap() (ver int64, g Getter) {
	// WARNING: Be sure to read the pointer value of st.state only once. If you
	// need multiple accesses, copy the pointer first.
	p := st.state

	return p.ver, p.root
}

// Gets the value stored at `path`, if any.
//
// If no value is stored at `path`, `rev` will be `Missing` and `value` will be
// nil.
//
// if `path` is a directory, `rev` will be `Dir` and `value` will be a list of
// entries.
//
// Otherwise, `rev` is the revision and `value[0]` is the body.
func (st *Store) Get(path string) (value []string, rev int64) {
	_, g := st.Snap()
	return g.Get(path)
}

func (st *Store) Stat(path string) (int32, int64) {
	_, g := st.Snap()
	return g.Stat(path)
}


// Apply all operations in the internal queue, even if there are gaps in the
// sequence (gaps will be treated as no-ops). This is only useful for
// bootstrapping a store from a point-in-time snapshot of another store.
func (st *Store) Flush() {
	st.flush <- true
}


// A convenience wrapper for NewWatch that returns only the channel. Useful for
// code that never needs to stop the Watch.
func (st *Store) Watch(glob *Glob) <-chan Event {
	return NewWatch(st, glob).C
}

// Arranges for w to receive notifications when mutations are applied to st.
// One event e will be received from w.C for each mutation iff
// glob.Match(e.Path).
//
// Notifications will not be sent for changes that were made by calling
// st.Flush.
func NewWatch(st *Store, glob *Glob) *Watch {
	rev, _ := st.Snap()
	w, err := NewWatchFrom(st, glob, rev+1)
	if err != nil {
		panic(err)
	}
	return w
}

// Arranges for w to receive notifications when mutations are applied to st.
// One event e will be received from w.C for each mutation iff
// glob.Match(e.Path).
//
// Notifications will not be sent for changes that were made by calling
// st.Flush.
//
// If `from` is less than any value passed to st.Clean, NewWatchFrom
// will return `ErrTooLate`.
func NewWatchFrom(st *Store, glob *Glob, from int64) (*Watch, os.Error) {
	ch := make(chan Event)
	return st.watchOn(glob, ch, from, math.MaxInt64)
}

func (st *Store) watchOn(glob *Glob, ch chan Event, from, to int64) (*Watch, os.Error) {
	if from < 1 {
		return nil, ErrTooLate
	}
	wt := &Watch{
		C:        ch,
		c:        ch,
		glob:     glob,
		from:     from,
		to:       to,
		shutdown: make(chan bool, 1),
	}
	st.watchCh <- wt
	head := st.head
	if head > from {
		wt.Stop()
		return nil, ErrTooLate
	}
	return wt, nil
}

// Returns a read-only chan that will receive a single event representing the
// change made at position `seqn`.
//
// If `seqn` is less than any value passed to st.Clean, Wait will return
// `ErrTooLate`.
func (st *Store) Wait(seqn int64) (<-chan Event, os.Error) {
	w, err := st.watchOn(Any, make(chan Event, 1), seqn, seqn+1)
	if err != nil {
		return nil, err
	}
	return w.C, nil
}

// Returns an immutable copy of `st` in which `path` exists as a regular file
// (not a dir). Waits for `path` to be set, if necessary.
//
// Returns nil, nil if the store is closed.
func (st *Store) SyncPath(path string) (Getter, os.Error) {
	glob, err := CompileGlob(path)
	if err != nil {
		return nil, err
	}

	wt := NewWatch(st, glob)
	defer wt.Stop()

	_, g := st.Snap()
	_, rev := g.Get(path)
	if rev != Dir && rev != Missing {
		return g, nil
	}

	for ev := range wt.C {
		if ev.IsSet() {
			return ev, nil
		}
	}

	return nil, nil
}

func (st *Store) Clean(seqn int64) {
	st.cleanCh <- seqn
}
