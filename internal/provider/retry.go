package provider

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"
)

// Retries is how many times a failed request is tried again (SPEC §4.5).
const Retries = 3

// maxRetryWait bounds the wait before a retry, Retry-After included.
const maxRetryWait = 60 * time.Second

// StreamRetry starts a response as p.Stream does, and tries again, up to
// Retries times, when the request fails before any of the response arrived
// in a way that may pass: a dropped connection, HTTP 429, 500, 502, 503,
// 504 or 529, or an overloaded error at the start of the stream. It waits
// as Retry-After says, or backs off exponentially with jitter, and tells
// onRetry, if set, before each wait. Once anything of the reply has
// arrived, a broken stream is the caller's: the reply on screen stays.
func StreamRetry(ctx context.Context, p Provider, req Request, onRetry func(Retrying)) (Stream, error) {
	for attempt := 0; ; attempt++ {
		s, err := p.Stream(ctx, req)
		if err == nil {
			var p *primed
			if p, err = prime(s); err == nil {
				return p, nil
			}
			s.Close()
		}
		if attempt == Retries || !Retryable(err) || ctx.Err() != nil {
			return nil, err
		}
		wait := waitBeforeRetry(attempt, err)
		if onRetry != nil {
			onRetry(Retrying{Attempt: attempt + 1, Of: Retries, Wait: wait, Err: err})
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, context.Cause(ctx)
		case <-t.C:
		}
	}
}

// primed is a stream whose first events were read ahead, to see that the
// response really started: the usage a provider reports up front, until
// the first event of the reply itself.
type primed struct {
	Stream
	ahead []Event
	err   error // what ended the stream right after them, if anything
}

// prime reads s up to the first event of the reply. An error before it
// is returned, for the caller to retry; the end of the stream there is an
// empty reply, passed on.
func prime(s Stream) (*primed, error) {
	p := &primed{Stream: s}
	for {
		ev, err := s.Recv()
		switch {
		case errors.Is(err, io.EOF):
			p.err = err
			return p, nil
		case err != nil:
			return nil, err
		}
		p.ahead = append(p.ahead, ev)
		if _, ok := ev.(Usage); !ok {
			return p, nil
		}
	}
}

func (p *primed) Recv() (Event, error) {
	if len(p.ahead) > 0 {
		ev := p.ahead[0]
		p.ahead = p.ahead[1:]
		return ev, nil
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.Stream.Recv()
}

// Retryable reports whether a failed request may succeed when sent again
// unchanged: the server was busy or failed, or the connection dropped. A
// refused connection, a timeout and a cancelled request are not: the
// server isn't there, it was given its time, or the user stopped it.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 429, 500, 502, 503, 504, 529:
			return true
		case 0: // inside the stream: Anthropic's kinds, and the Responses API's codes
			switch apiErr.Type {
			case "overloaded_error", "api_error", "server_error", "rate_limit_exceeded":
				return true
			}
			return false
		}
		return false
	}
	var idle *IdleError
	if errors.As(err, &idle) || errors.Is(err, ErrIncomplete) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// waitBeforeRetry is retryWait; tests shorten it.
var waitBeforeRetry = retryWait

// retryWait is how long to wait before retry attempt+1: what the server
// asked for, or 1, 2, 4 … seconds with up to half again of jitter.
func retryWait(attempt int, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, maxRetryWait)
	}
	base := time.Second << attempt
	return min(base+time.Duration(rand.Int64N(int64(base)/2+1)), maxRetryWait)
}
