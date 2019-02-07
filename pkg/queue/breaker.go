/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package queue

import (
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

var (
	// ErrAddCapacity indicates that not all capacity could be added to the breaker.
	ErrAddCapacity = errors.New("failed to add all capacity to the breaker")
	// ErrReduceCapacity indicates that there's not enough capacity to be reduced.
	ErrReduceCapacity = errors.New("the capacity that is released must be <= to added capacity")
	// ErrRelease indicates that Release was called more often than Acquire.
	ErrRelease = errors.New("semaphore release error: returned tokens must be <= acquired tokens")
)

// BreakerParams defines the parameters of the breaker.
type BreakerParams struct {
	QueueDepth      int32
	MaxConcurrency  int32
	InitialCapacity int32
	Logger          *zap.SugaredLogger
}

type token struct{}

// Breaker is a component that enforces a concurrency limit on the
// execution of a function. It also maintains a queue of function
// executions in excess of the concurrency limit. Function call attempts
// beyond the limit of the queue are failed immediately.
type Breaker struct {
	pendingRequests chan token
	sem             *Semaphore
	logger          *zap.SugaredLogger
}

// NewBreaker creates a Breaker with the desired queue depth,
// concurrency limit and initial capacity.
func NewBreaker(params BreakerParams) *Breaker {
	if params.QueueDepth <= 0 {
		panic(fmt.Sprintf("Queue depth must be greater than 0. Got %v.", params.QueueDepth))
	}
	if params.MaxConcurrency < 0 {
		panic(fmt.Sprintf("Max concurrency must be 0 or greater. Got %v.", params.QueueDepth))
	}
	if params.InitialCapacity < 0 || params.InitialCapacity > params.MaxConcurrency {
		panic(fmt.Sprintf("Initial capacity must be between 0 and max concurrency. Got %v.", params.InitialCapacity))
	}
	sem := NewSemaphore(params.MaxConcurrency, params.InitialCapacity)
	return &Breaker{
		pendingRequests: make(chan token, params.QueueDepth+params.MaxConcurrency),
		sem:             sem,
		logger:          params.Logger,
	}
}

// Maybe conditionally executes thunk based on the Breaker concurrency
// and queue parameters. If the concurrency limit and queue capacity are
// already consumed, Maybe returns immediately without calling thunk. If
// the thunk was executed, Maybe returns true, else false.
func (b *Breaker) Maybe(thunk func()) bool {
	var t token
	select {
	default:
		// Pending request queue is full.  Report failure.
		return false
	case b.pendingRequests <- t:
		// Pending request has capacity.
		// Wait for capacity in the active queue.
		b.sem.Acquire()
		// Defer releasing capacity in the active and pending request queue.
		defer func() {
			if err := b.sem.Release(); err != nil {
				b.logger.Errorw("Error while releasing a semaphore:", zap.Error(err))
			}
			<-b.pendingRequests
		}()
		// Do the thing.
		thunk()
		// Report success
		return true
	}
}

// UpdateConcurrency updates the maximum number of in-flight requests.
func (b *Breaker) UpdateConcurrency(size int32) error {
	return b.sem.UpdateCapacity(size)
}

// Capacity returns the number of allow in-flight requests on this breaker.
func (b *Breaker) Capacity() int32 {
	return b.sem.Capacity()
}

// NewSemaphore creates a semaphore with the desired maximal and initial capacity.
// Maximal capacity is the size of the buffered channel, it defines maximum number of tokens
// in the rotation. Attempting to add more capacity then the max will result in error.
// Initial capacity is the initial number of free tokens.
func NewSemaphore(maxCapacity, initialCapacity int32) *Semaphore {
	if initialCapacity < 0 || initialCapacity > maxCapacity {
		panic(fmt.Sprintf("Initial capacity must be between 0 and maximal capacity. Got %v.", initialCapacity))
	}
	queue := make(chan token, maxCapacity)
	sem := Semaphore{queue: queue, maxCapacity: maxCapacity}
	if initialCapacity > 0 {
		sem.UpdateCapacity(initialCapacity)
	}
	return &sem
}

// Semaphore is an implementation of a semaphore based on Go channels.
// The presence of elements in the `queue` buffered channel correspond to available tokens.
// Hence the max number of tokens to hand out equals to the size of the channel.
// `capacity` defines the current number of tokens in the rotation.
type Semaphore struct {
	queue       chan token
	token       token
	reducers    int32
	capacity    int32
	maxCapacity int32
	mux         sync.Mutex
}

// Acquire receives the token from the semaphore, potentially blocking.
func (s *Semaphore) Acquire() {
	<-s.queue
}

// Release potentially puts the token back to the queue.
// If the semaphore capacity was reduced in between and is not yet reflected,
// we remove the tokens from the rotation instead of returning them back.
func (s *Semaphore) Release() error {
	s.mux.Lock()
	defer s.mux.Unlock()

	if s.reducers > 0 {
		s.capacity--
		s.reducers--
		return nil
	}

	// We want to make sure releasing a token is always non-blocking.
	select {
	case s.queue <- s.token:
		return nil
	default:
		// This should never happen.
		return ErrRelease
	}
}

// UpdateCapacity updates the capacity of the semaphore to the desired
// size.
func (s *Semaphore) UpdateCapacity(size int32) error {
	if size < 0 {
		return ErrReduceCapacity
	}

	s.mux.Lock()
	defer s.mux.Unlock()

	if s.effectiveCapacity() == size {
		return nil
	}

	if size > s.maxCapacity {
		return ErrAddCapacity
	}

	// Add capacity until we reach size, potentially consuming
	// outstanding reducers first.
	for s.effectiveCapacity() < size {
		if s.reducers > 0 {
			s.reducers--
		} else {
			select {
			case s.queue <- s.token:
				s.capacity++
			default:
				// This indicates that we're operating close to
				// MaxCapacity and returned more tokens than we
				// acquired.
				return ErrAddCapacity
			}
		}
	}

	// Reduce capacity until we reach size, potentially adding
	// new reducers if the queue channel is empty because of
	// requests in-flight.
	for s.effectiveCapacity() > size {
		select {
		case <-s.queue:
			s.capacity--
		default:
			s.reducers++
		}
	}

	return nil
}

// effectiveCapacity is the capacity with reducers taken into account.
// `mux` must be held to call it.
func (s *Semaphore) effectiveCapacity() int32 {
	return s.capacity - s.reducers
}

// Capacity is the effective capacity after taking reducers into
// account.
func (s *Semaphore) Capacity() int32 {
	s.mux.Lock()
	defer s.mux.Unlock()

	return s.effectiveCapacity()
}
