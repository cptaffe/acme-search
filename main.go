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
	q0      int
	prompt  string
	q0s     []int     // q0 of results
	results []*Result // results
	win     *acme.Win
}

func search(ctx context.Context, query string, ch chan<- string) error {
	defer close(ch)

	// Debounce
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}

	cmd := exec.CommandContext(ctx, "L", "sym", "-p", query)
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- scanner.Text():
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning: %w", err)
	}
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("wait L sym: %w", err)
	}
	return nil
}

type Result struct {
	Text  string
	Addr  string
	Score fuzzy.Score
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
	ch := make(chan string)
	query := s.prompt
	go func() {
		err := search(ctx, query, ch)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("search: %v", err)
		}
	}()
	go func() {
		var results ResultHeap
		heap.Init(&results)
		shouldRender := time.Now().Add(100 * time.Millisecond)

		// Debounce render
		render := func() error {
			window := min(results.Len(), 20)
			topN := make([]*Result, window)
			for i := range window {
				topN[(window-1)-i] = heap.Pop(&results).(*Result)
			}
			err := s.writeResults(topN)
			if err != nil {
				return fmt.Errorf("write line: %w", err)
			}
			// Reset timer
			shouldRender = time.Now().Add(100 * time.Millisecond)
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
				if result == "" {
					err := render()
					if err != nil {
						log.Printf("render: %v", err)
					}
					return
				}
				parts := strings.SplitN(result, ":", 3)
				var res Result
				switch len(parts) {
				case 3:
					res.Addr = parts[0] + ":" + parts[1]
					res.Text = parts[2]
				case 2:
					res.Addr = parts[0]
					res.Text = parts[1]
				case 1:
					res.Text = parts[0]
				}
				res.Score = fuzzy.Match(query, res.Text)
				// Only show positive scores
				if res.Score > 0 {
					heap.Push(&results, &res)
				}
			}
		}
	}()
}

func (s *Search) writeResults(results []*Result) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.results = results
	s.q0s = make([]int, len(results))
	var sb strings.Builder
	for i, result := range results {
		s.q0s[i] = sb.Len()
		fmt.Fprintf(&sb, "%f: %s\n", result.Score, result.Text)
	}
	err := s.win.Addr("#%d,#%d", 0, s.q0-2)
	if err != nil {
		return fmt.Errorf("addr: %w")
	}
	_, err = s.win.Write("data", []byte(sb.String()))
	if err != nil {
		return fmt.Errorf("write: %w")
	}
	s.q0 = 2 + sb.Len()
	return nil
}

func (s *Search) insert(ctx context.Context, q0, q1 int, text string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	// If an edit occurs earlier in the document, update the prompt line position
	if q0 < s.q0 {
		s.q0 += q1 - q0
		return nil
	}
	// Insert within prompt line
	s.prompt = s.prompt[:q0-s.q0] + text + s.prompt[q0-s.q0:]

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
	// Deletion which ends before the prompt
	if q0 < s.q0 {
		s.q0 -= q1 - q0
		return nil
	}
	// Deletion which includes the prompt
	if q1 < s.q0 {
		// Distance between beginning of delete and beginning of prompt
		diff := s.q0 - q0
		// Prompt now starts at beginning of deletion
		s.q0 -= diff
		// Update delete to start at beginning of prompt
		q0 = s.q0
		// Adjust end to account for movement of prompt
		q1 -= diff
	}
	// Delete within prompt line
	s.prompt = s.prompt[:q0-s.q0] + s.prompt[q1-s.q0:]

	if s.cancel != nil {
		s.cancel() // Cancel previous search
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.Search(ctx)
	return nil
}

func (s *Search) Plumb(q0 int) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	i, _ := slices.BinarySearch(s.q0s, q0)

	cmd := exec.Command("plumb", s.results[i-1].Addr)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("plumb: %w", err)
	}
	return nil
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
				if e.OrigQ0 < s.q0 {
					err := s.Plumb(e.OrigQ0)
					if err != nil {
						return err
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

	s := &Search{q0: 2, prompt: "", win: win}

	_, err = win.Write("body", []byte{'>', ' '})
	if err != nil {
		log.Printf("new acme win: %v", err)
		return
	}

	defer cancel()
	err = s.EventLoop(ctx)
	if err != nil {
		log.Printf("new acme win: %v", err)
		return
	}
}
