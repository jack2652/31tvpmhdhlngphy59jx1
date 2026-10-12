package server

import (
	"context"
	"time"
)

type levelsCacheFlight struct {
	done        chan struct{}
	waiters     int
	emptyResult bool
}

type levelsCacheTimings struct {
	cacheRead  time.Duration
	cacheWrite time.Duration
	flightWait time.Duration
	build      time.Duration
}

func (s *Service) getOrBuildLevels(ctx context.Context, key string, maxEntries int, build func() map[string]any) (map[string]any, error) {
	return s.getOrBuildLevelsTimed(ctx, key, maxEntries, build, nil)
}

func (s *Service) getOrBuildLevelsTimed(ctx context.Context, key string, maxEntries int, build func() map[string]any, timings *levelsCacheTimings) (map[string]any, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		readStarted := time.Now()
		cached := s.readLevelsCache(key)
		if timings != nil {
			timings.cacheRead += time.Since(readStarted)
		}
		if cached != nil {
			return cached, nil
		}

		flight, leader := s.joinLevelsFlight(key)
		if !leader {
			waitStarted := time.Now()
			select {
			case <-flight.done:
				if timings != nil {
					timings.flightWait += time.Since(waitStarted)
				}
				s.mu.Lock()
				flight.waiters--
				s.mu.Unlock()
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if cached := s.readLevelsCache(key); cached != nil {
					return cached, nil
				}
				if flight.emptyResult {
					return nil, nil
				}
				continue
			case <-ctx.Done():
				s.mu.Lock()
				flight.waiters--
				s.mu.Unlock()
				return nil, ctx.Err()
			}
		}

		var computed map[string]any
		var ctxErr error
		func() {
			defer s.finishLevelsFlight(key, flight)
			if err := ctx.Err(); err != nil {
				ctxErr = err
				return
			}
			readStarted := time.Now()
			cached := s.readLevelsCache(key)
			if timings != nil {
				timings.cacheRead += time.Since(readStarted)
			}
			if cached != nil {
				computed = cached
				return
			}
			buildStarted := time.Now()
			computed = build()
			if timings != nil {
				timings.build += time.Since(buildStarted)
			}
			flight.emptyResult = computed == nil
			if computed != nil {
				writeStarted := time.Now()
				_ = s.store.PutAnalysisCache(key, computed, maxEntries)
				if timings != nil {
					timings.cacheWrite += time.Since(writeStarted)
				}
			}
			ctxErr = ctx.Err()
		}()
		if ctxErr != nil {
			return nil, ctxErr
		}
		return computed, nil
	}
}

func (s *Service) readLevelsCache(key string) map[string]any {
	value, found, err := s.store.GetAnalysisCache(key)
	if err != nil || !found {
		return nil
	}
	computed, _ := value.(map[string]any)
	return computed
}

func (s *Service) joinLevelsFlight(key string) (*levelsCacheFlight, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.levelsFlights == nil {
		s.levelsFlights = make(map[string]*levelsCacheFlight)
	}
	if flight := s.levelsFlights[key]; flight != nil {
		flight.waiters++
		return flight, false
	}
	flight := &levelsCacheFlight{done: make(chan struct{})}
	s.levelsFlights[key] = flight
	return flight, true
}

func (s *Service) finishLevelsFlight(key string, flight *levelsCacheFlight) {
	s.mu.Lock()
	if s.levelsFlights[key] == flight {
		delete(s.levelsFlights, key)
	}
	close(flight.done)
	s.mu.Unlock()
}
