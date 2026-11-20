/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package blueprint

import (
	"sync/atomic"
	"time"
)

type moduleProgressReporter struct {
	completed atomic.Int64
	stop      chan struct{}
	done      chan struct{}
}

func newModuleProgressReporter(total int, hook func(int, int)) *moduleProgressReporter {
	p := &moduleProgressReporter{stop: make(chan struct{}), done: make(chan struct{})}
	hook(0, total)
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		last := 0
		for {
			select {
			case <-ticker.C:
				if completed := int(p.completed.Load()); completed != last {
					hook(completed, total)
					last = completed
				}
			case <-p.stop:
				hook(int(p.completed.Load()), total)
				return
			}
		}
	}()
	return p
}

func (p *moduleProgressReporter) finish() {
	close(p.stop)
	<-p.done
}
