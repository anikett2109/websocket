// Package trades keeps a bounded ring buffer of the most recent trades.
// Not safe for concurrent use; the market package owns locking.
package trades

import "cryptofeed/internal/model"

type Store struct {
	buf  []model.Trade
	next int // write position
	n    int // number of valid entries
}

func NewStore(capacity int) *Store {
	return &Store{buf: make([]model.Trade, capacity)}
}

func (s *Store) Add(t model.Trade) {
	s.buf[s.next] = t
	s.next = (s.next + 1) % len(s.buf)
	if s.n < len(s.buf) {
		s.n++
	}
}

func (s *Store) Len() int { return s.n }

// at returns the i-th newest trade (0 = newest).
func (s *Store) at(i int) model.Trade {
	return s.buf[(s.next-1-i+len(s.buf))%len(s.buf)]
}

// Latest returns up to n trades, newest first.
func (s *Store) Latest(n int) []model.Trade {
	n = min(n, s.n)
	out := make([]model.Trade, n)
	for i := range out {
		out[i] = s.at(i)
	}
	return out
}

// Range returns trades with from <= ts <= to (0 = unbounded), newest first, at most limit.
func (s *Store) Range(from, to int64, limit int) []model.Trade {
	var out []model.Trade
	for i := 0; i < s.n && len(out) < limit; i++ {
		t := s.at(i)
		if to > 0 && t.TS > to {
			continue
		}
		if from > 0 && t.TS < from {
			break // older entries are all earlier still
		}
		out = append(out, t)
	}
	return out
}

// Oldest returns the timestamp of the oldest retained trade (0 if empty).
func (s *Store) Oldest() int64 {
	if s.n == 0 {
		return 0
	}
	return s.at(s.n - 1).TS
}
