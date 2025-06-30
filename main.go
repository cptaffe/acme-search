package main

import (
	"bufio"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"9fans.net/go/acme"
	"github.com/cptaffe/acme-search/fuzzy"
)

type Search struct {
	lock    sync.Mutex
	cancel  context.CancelFunc
	prompt  string
	query   string
	q0s     []int     // q0 of results
	results []*Result // results
	win     *acme.Win
}

const DefaultFlags = "sw" // symbols and windows

// Flags can enable additional functionality
func (s *Search) Flags() string {
	parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(s.query, s.prompt)), "+", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return DefaultFlags
}

func (s *Search) Query() string {
	return strings.SplitN(strings.TrimSpace(strings.TrimPrefix(s.query, s.prompt)), "+", 2)[0]
}

func commandSource(ctx context.Context, command []string, ch chan<- *Result) error {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	r, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	defer r.Close()
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start L sym: %w", err)
	}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var res Result
		parts := strings.SplitN(scanner.Text(), ":", 4)
		switch len(parts) {
		case 4:
			res.Addr = parts[0] + ":" + parts[1] + ":" + parts[2]
			res.Text = parts[3]
		case 3:
			res.Addr = parts[0] + ":" + parts[1]
			res.Text = parts[2]
		case 2:
			res.Addr = parts[0]
			res.Text = parts[1]
		case 1:
			res.Text = parts[0]
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- &res:
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning: %w", err)
	}
	err = cmd.Wait()
	if err != nil {
		select {
		case <-ctx.Done():
			return ctx.Err() // likely `signal: killed` caused by cancelation
		default:
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

type Result struct {
	Text  string
	Addr  string
	Score fuzzy.Score
}

func (r Result) Equals(o *Result) bool {
	// Don't compare floats
	return r.Text == o.Text && r.Addr == o.Addr
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
	// Wait 100ms before we start
	select {
	case <-ctx.Done():
		return
	case <-time.After(100 * time.Millisecond):
	}

	ch := make(chan *Result)
	query := s.Query()
	flags := s.Flags()
	var wg sync.WaitGroup
	for _, flag := range flags {
		wg.Add(1)
		switch flag {
		case 's': // symbols
			go func() {
				defer wg.Done()
				err := commandSource(ctx, []string{"L", "sym", "-p", query}, ch)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("L sym: %v", err)
				}
			}()
		case 'w': // windows
			go func() {
				defer wg.Done()
				err := indexSource(ctx, ch)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("windows: %v", err)
				}
			}()
		case 'g': // regexp
			go func() {
				defer wg.Done()
				err := commandSource(ctx, []string{"rg", query}, ch)
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
		// Wait 100ms before we render -- total 200ms delay
		shouldRender := time.Now().Add(100 * time.Millisecond)

		// Debounce render
		render := func() error {
			// Render at least once, avoid re-rendering same results
			currentLen := results.Len()
			if hasRendered && currentLen == lastLen {
				hasRendered = true
				return nil
			}

			topN := make([]*Result, max(results.Len(), 20))
			i := 0
			for i < 20 && results.Len() > 0 {
				topN[i] = heap.Pop(&results).(*Result)
				if i == 0 || !topN[i-1].Equals(topN[i]) {
					i++ // Only advance if not a duplicate
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
			shouldRender = time.Now().Add(100 * time.Millisecond)
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
			case result := <-ch:
				if result == nil {
					err := render()
					if err != nil {
						log.Printf("render: %v", err)
					}
					return
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

func (s *Search) writeResults(ctx context.Context, results []*Result) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	// If the context is canceled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.results = results
	s.q0s = make([]int, len(results))
	var sb strings.Builder

	// Fix query line newline, if deleted
	if !strings.HasSuffix(s.query, "\n") {
		s.query += "\n"
	}
	sb.WriteString(s.query)

	for i, result := range results {
		s.q0s[i] = sb.Len() - 1
		fmt.Fprintf(&sb, "%f: %s\n", result.Score, result.Text)
	}
	err := s.win.Addr("0,$")
	if err != nil {
		return fmt.Errorf("addr: %w", err)
	}
	_, err = s.win.Write("data", []byte(sb.String()))
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// Place the cursor back at the end of the prompt line
	err = s.win.Addr("#%d", len(s.query)-1)
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
		// TODO: Update s.q0s
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
		// TODO: Update s.q0s
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
	if len(s.q0s) == 0 || q0 < s.q0s[0] {
		return false, nil
	}

	i, _ := slices.BinarySearch(s.q0s, q0)
	addr := s.results[i-1].Addr
	if addr == "" {
		return false, nil
	}

	cmd := exec.Command("plumb", addr)
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
