// TODO: Mark clean when all results are in
// TODO: Update working directory of sourceCommand based on window name changes
// TODO: Look on file name should not redirect to last symbol of previous file (need q0 for file lines)
// TODO: Tree mode: display results like tree, grouped by shared parent -- great with +f for find mode (match on file name, not contents)
package main

import (
	"bufio"
	"cmp"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"9fans.net/go/acme"
	"github.com/cptaffe/acme-search/fuzzy"
)

// Range represents a half-open range [start, end)
type Range struct {
	Start int
	End   int
}

func (r Range) Compare(i int) int {
	if i < r.Start {
		return 1 // before
	}
	if i >= r.End {
		return -1 // after
	}
	return 0 // within
}

type Search struct {
	lock    sync.Mutex
	cancel  context.CancelFunc
	prompt  string
	query   string
	ranges  []Range   // ranges of results
	results []*Result // results
	win     *acme.Win
}

type Flag rune

const (
	FlagSymbols Flag = 's' // Search symbols known to any language servers
	FlagWindows Flag = 'w' // Search open windows by name
	FlagGrep    Flag = 'g' // Search contents of files recursively using rg, see also: plan9port/bin/g
	FlagFiles   Flag = 'f' // Search files recursively by name

	MaxLineLength    string        = "2048"
	MaxResults       int           = 100
	DebounceDuration time.Duration = 100 * time.Millisecond
)

var DefaultFlags []Flag = []Flag{FlagSymbols, FlagWindows, FlagGrep}

// Flags can enable additional functionality
func (s *Search) Flags() []Flag {
	parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(s.query, s.prompt)), "+", 2)
	if len(parts) == 2 {
		flags := make([]Flag, len(parts[1]))
		for i, r := range parts[1] {
			flags[i] = Flag(r)
		}
		return flags
	}
	return DefaultFlags
}

func (s *Search) Query() string {
	return strings.SplitN(strings.TrimSpace(strings.TrimPrefix(s.query, s.prompt)), "+", 2)[0]
}

var twoColonRangeRegexp = regexp.MustCompile(`(.*):([0-9]+).([0-9]+)[:,]([0-9]+).([0-9]+)[: ](.*)`)
var twoColonAddrRegexp = regexp.MustCompile(`(.*):([0-9]+)[:,]([0-9]+)[: ](.*)`)

func commandSource(ctx context.Context, command []string, ch chan<- *Result) error {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	r, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	defer r.Close()
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// TODO: Allow lines longer than 64k, use regexp.MatchReader with regexp incorporating : prefix grammar and max lengths, then consume up to newline or EOF.

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		var res Result
		if matches := twoColonRangeRegexp.FindStringSubmatch(line); matches != nil {
			res.Addr = &Addr{
				File:       matches[1],
				FromLine:   matches[2],
				FromColumn: matches[3],
				ToLine:     matches[4],
				ToColumn:   matches[5],
			}
			res.Text = matches[6]
		} else if matches := twoColonAddrRegexp.FindStringSubmatch(line); matches != nil {
			res.Addr = &Addr{
				File:       matches[1],
				FromLine:   matches[2],
				FromColumn: matches[3],
			}
			res.Text = matches[4]
		} else {
			res.Text = line
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- &res:
		}
	}
	err = scanner.Err()
	if err != nil {
		return fmt.Errorf("scanning: %w", err)
	}
	err = cmd.Wait()
	if err != nil {
		select {
		case <-ctx.Done():
			return ctx.Err() // likely `signal: killed` caused by cancelation
		default:
			// e.g. 'exit status 1', representing no results found
			if _, ok := err.(*exec.ExitError); ok {
				return nil // swallow
			}
			return fmt.Errorf("wait: %w", err)
		}
	}
	return nil
}

func indexSource(ctx context.Context, ch chan<- *Result) error {
	windows, err := acme.Windows()
	if err != nil {
		return fmt.Errorf("windows: %w", err)
	}
	for _, win := range windows {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- &Result{Text: win.Name}:
		}
	}
	return nil
}

type Addr struct {
	File       string
	FromLine   string
	FromColumn string
	ToLine     string
	ToColumn   string
}

func (a Addr) String() string {
	s := a.File
	if a.FromLine != "" {
		s += ":" + a.FromLine
		if a.FromColumn != "" {
			s += "." + a.FromColumn
		}
		if a.ToLine != "" {
			s += "," + a.ToLine
			if a.ToColumn != "" {
				s += "." + a.ToColumn
			}
		}
	}
	return s
}

type Result struct {
	Text  string
	Addr  *Addr
	Score fuzzy.Score
}

func (r Result) Equals(o *Result) bool {
	// Don't compare floats
	return r.Text == o.Text && *r.Addr == *o.Addr
}

func (r Result) String() string {
	return fmt.Sprintf("%s\n%s\n", r.Addr, r.Text)
}

type ResultHeap []*Result

func (h ResultHeap) Len() int           { return len(h) }
func (h ResultHeap) Less(i, j int) bool { return h[i].Score > h[j].Score }
func (h ResultHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *ResultHeap) Push(x any) {
	*h = append(*h, x.(*Result))
}

func (h *ResultHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

func (s *Search) Search(ctx context.Context) {
	// Wait duration before we start
	select {
	case <-ctx.Done():
		return
	case <-time.After(DebounceDuration):
	}

	// Use the search window's path as the search root
	// TODO: Track changes here via Ki events
	path := "."
	data, err := s.win.ReadAll("tag")
	if err != nil {
		log.Printf("read tag: %v", err)
		return
	}
	tag := string(data)
	i := strings.Index(tag, " Del Snarf ")
	if i == -1 {
		log.Printf("cannot determine filename in tag %q", tag)
	}
	path = filepath.Dir(tag[:i])

	ch := make(chan *Result)
	query := s.Query()
	flags := s.Flags()
	var wg sync.WaitGroup
	for _, flag := range flags {
		wg.Add(1)
		switch flag {
		case FlagSymbols:
			go func() {
				defer wg.Done()
				err := commandSource(ctx, []string{"L", "sym", "-p", query}, ch)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("L sym: %v", err)
				}
			}()
		case FlagWindows:
			go func() {
				defer wg.Done()
				err := indexSource(ctx, ch)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("windows: %v", err)
				}
			}()
		case FlagGrep:
			go func() {
				defer wg.Done()
				err := commandSource(ctx, []string{"rg", "--max-columns", MaxLineLength, query, path}, ch)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("ripgrep: %v", err)
				}
			}()
		case FlagFiles:
			go func() {
				defer wg.Done()
				err := commandSource(ctx, []string{"rg", "--max-columns", MaxLineLength, "--iglob", "*" + query + "*", "--files", path}, ch)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("ripgrep: %v", err)
				}
			}()
		default:
			wg.Done()
			log.Printf("unknown flag: %c", flag)
		}
	}
	// Close channel only when all writers are finished
	go func() {
		wg.Wait()
		close(ch)
	}()
	go func() {
		var results ResultHeap
		hasRendered := false
		heap.Init(&results)
		lastLen := results.Len()
		// Wait duration before we render -- total 2*duration delay
		shouldRender := time.Now().Add(DebounceDuration)

		// Debounce render
		render := func() error {
			// Render at least once, avoid re-rendering same results
			currentLen := results.Len()
			if hasRendered && currentLen == lastLen {
				return nil
			}
			hasRendered = true

			topN := make([]*Result, max(results.Len(), MaxResults))
			seenAtAddr := make(map[Addr]struct{})
			seenInFile := make(map[string]int)
			i := 0
			for i < MaxResults && results.Len() > 0 {
				result := heap.Pop(&results).(*Result)
				topN[i] = result
				if result.Addr == nil {
					i++
					continue
				}
				_, isDup := seenAtAddr[*result.Addr]
				if !isDup {
					seenAtAddr[*result.Addr] = struct{}{}
				}
				count := seenInFile[result.Addr.File]
				seenInFile[result.Addr.File] = count + 1
				if i == 0 || (count < 5 && !isDup) {
					i++
				}
			}
			topN = topN[:i] // truncate unused slots
			// TODO: Use btree to avoid mutation on read
			for _, result := range topN {
				heap.Push(&results, result)
			}
			err := s.writeResults(ctx, topN)
			if err != nil {
				return fmt.Errorf("write line: %w", err)
			}

			// Reset timer
			shouldRender = time.Now().Add(DebounceDuration)
			lastLen = results.Len() // may have changed
			return nil
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(shouldRender)):
				err := render()
				if err != nil {
					log.Printf("render: %v", err)
					return
				}
			case result, ok := <-ch:
				if !ok {
					err := render()
					if err != nil {
						log.Printf("render: %v", err)
					}
					return
				}

				// Rewrite path as relative
				if result.Addr != nil {
					result.Addr.File, _ = strings.CutPrefix(result.Addr.File, path+"/")
				}

				result.Score = fuzzy.Match(query, result.Text)
				// Only show positive scores
				if result.Score > 0 {
					heap.Push(&results, result)
				}
			}
		}
	}()
}

type Group struct {
	Name    string // optional name for group
	Results []*Result
}

func (s *Search) writeResults(ctx context.Context, results []*Result) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	// If the context is canceled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Get current cursor position
	err := s.win.Ctl("addr=dot")
	if err != nil {
		return fmt.Errorf("addr=dot: %w", err)
	}
	q0, q1, err := s.win.ReadAddr()
	if err != nil {
		return fmt.Errorf("read addr: %w", err)
	}

	s.results = make([]*Result, len(results))
	s.ranges = make([]Range, len(results))
	var sb strings.Builder

	// Fix query line newline, if deleted
	if !strings.HasSuffix(s.query, "\n") {
		s.query += "\n"
	}
	sb.WriteString(s.query)

	// TODO: not all results have files
	var groups []*Group
	for _, result := range results {
		if result.Addr == nil {
			groups = append(groups, &Group{Results: []*Result{result}})
			continue
		}
		file := result.Addr.File
		for _, group := range groups {
			if group.Name == file {
				i, _ := slices.BinarySearchFunc(group.Results, result, func(a *Result, b *Result) int {
					if a.Addr == nil || b.Addr == nil || a.Addr.FromLine == "" || b.Addr.FromLine == "" {
						return cmp.Compare(b.Score, a.Score) // higher scores at top
					}
					i, _ := strconv.Atoi(a.Addr.FromLine)
					j, _ := strconv.Atoi(b.Addr.FromLine)
					return cmp.Compare(i, j) // lower line numbers at top
				})
				group.Results = slices.Insert(group.Results, i, result)
				goto L
			}
		}
		groups = append(groups, &Group{Name: file, Results: []*Result{result}})
	L:
	}

	i := 0
	for _, group := range groups {
		if group.Name != "" {
			fmt.Fprintf(&sb, "%s\n", group.Name)
		}
		for _, result := range group.Results {
			s.results[i] = result // place in updated order
			start := sb.Len() - 1
			addr := result.Addr
			if addr != nil && addr.FromLine != "" {
				fmt.Fprintf(&sb, "%-5s ", addr.FromLine)
			}
			fmt.Fprintf(&sb, "%s\n", result.Text)
			s.ranges[i] = Range{start, sb.Len() - 1}
			i++
		}
	}
	err = s.win.Addr("0,$")
	if err != nil {
		return fmt.Errorf("addr: %w", err)
	}
	_, err = s.win.Write("data", []byte(sb.String()))
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// Place the cursor back at the end of the prompt line
	err = s.win.Addr("#%d,#%d", min(q0, len(s.query)-1), min(q1, len(s.query)-1))
	if err != nil {
		return fmt.Errorf("addr: %w", err)
	}
	err = s.win.Ctl("dot=addr")
	if err != nil {
		return fmt.Errorf("dot=addr: %v", err)
	}
	// Scroll the prompt line into view
	err = s.win.Ctl("show")
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	return nil
}

func (s *Search) insert(ctx context.Context, q0, q1 int, text string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	// If an edit occurs after the query line
	if q0 > len(s.query) {
		// TODO: Update s.ranges
		return nil
	}
	// Insert within query line
	s.query = s.query[:q0] + text + s.query[q0:]

	if s.cancel != nil {
		s.cancel() // Cancel previous search
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.Search(ctx)
	return nil
}

func (s *Search) delete(ctx context.Context, q0, q1 int) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	// Deletion which starts after the query line
	if q0 > len(s.query) {
		// TODO: Update s.ranges
		return nil
	}
	if q1 > len(s.query) {
		// Update delete to end at end of query
		q1 = len(s.query)
	}

	// Delete within query line
	s.query = s.query[:q0] + s.query[q1:]

	if s.cancel != nil {
		s.cancel() // Cancel previous search
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.Search(ctx)
	return nil
}

func (s *Search) Plumb(q0 int) (bool, error) {
	// TODO: right clicking the very beginning of the first line fails to plumb
	if len(s.ranges) == 0 || q0 < s.ranges[0].Start {
		return false, nil
	}

	i, found := slices.BinarySearchFunc(s.ranges, q0, func(r Range, q0 int) int { return r.Compare(q0) })
	if !found {
		return false, nil
	}

	addr := s.results[i].Addr
	if addr == nil {
		return false, nil
	}

	cmd := exec.Command("plumb", addr.String())
	err := cmd.Run()
	if err != nil {
		return true, fmt.Errorf("plumb: %w", err)
	}
	return true, nil
}

func (s *Search) EventLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-s.win.EventChan():
			if e == nil {
				return nil // window closed
			}
			// log.Printf("%c%c%d %d %d %d %s", e.C1, e.C2, e.OrigQ0, e.OrigQ1, e.Nr, e.Flag, e.Text)
			// See: win.EventLoop
			switch e.C2 {
			// Unblock standard window operations
			case 'x', 'X':
				s.win.WriteEvent(e)
			case 'l', 'L': // look
				if e.OrigQ0 > len(s.query) {
					ok, err := s.Plumb(e.OrigQ0)
					if err != nil {
						return err
					}
					if !ok {
						s.win.WriteEvent(e)
					}
					break
				}
				s.win.WriteEvent(e)
			default:
				switch e.C1 {
				case 'K':
					// Ignore tag events
					if unicode.IsLower(e.C2) {
						continue
					}

					switch e.C2 {
					case 'I':
						err := s.insert(ctx, e.OrigQ0, e.OrigQ1, string(e.Text))
						if err != nil {
							return fmt.Errorf("insert: %w", err)
						}
					case 'D':
						err := s.delete(ctx, e.OrigQ0, e.OrigQ1)
						if err != nil {
							return fmt.Errorf("delete: %w", err)
						}
					}
				}
			}
		}
	}
}

func (s *Search) WritePrompt() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	line := fmt.Sprintf("%s\n", s.prompt)
	s.query = line
	_, err := s.win.Write("body", []byte(line))
	if err != nil {
		fmt.Errorf("write: %w", err)
	}
	s.query = line
	err = s.win.Addr("#2")
	if err != nil {
		fmt.Errorf("addr: %w", err)
	}
	err = s.win.Ctl("dot=addr")
	if err != nil {
		return fmt.Errorf("dot=addr: %w", err)
	}
	err = s.win.Ctl("focus")
	if err != nil {
		return fmt.Errorf("focus: %w", err)
	}
	return err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	win, err := acme.New()
	if err != nil {
		log.Printf("new acme win: %v", err)
		return
	}
	defer win.CloseFiles()

	pwd, err := os.Getwd()
	if err != nil {
		log.Printf("pwd: %v", err)
		return
	}

	err = win.Ctl("name %s/+Search", pwd)
	if err != nil {
		log.Printf("new acme win: %v", err)
		return
	}

	s := &Search{prompt: "> ", win: win}
	s.WritePrompt()
	if err != nil {
		log.Printf("write prompt: %v", err)
		return
	}

	defer cancel()
	err = s.EventLoop(ctx)
	if err != nil {
		log.Printf("new acme win: %v", err)
		return
	}
}
