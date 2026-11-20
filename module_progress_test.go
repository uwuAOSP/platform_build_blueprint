/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package blueprint

import (
	"sync"
	"testing"
)

func TestModuleProgressReporterCountsCompletions(t *testing.T) {
	var counts [][2]int
	p := newModuleProgressReporter(1000, func(done, total int) {
		counts = append(counts, [2]int{done, total})
	})
	var workers sync.WaitGroup
	for i := 0; i < 10; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				p.completed.Add(1)
			}
		}()
	}
	workers.Wait()
	p.finish()
	if counts[0] != [2]int{0, 1000} || counts[len(counts)-1] != [2]int{1000, 1000} {
		t.Fatalf("incorrect initial/final counts: %v", counts)
	}
	for i := 1; i < len(counts); i++ {
		if counts[i][0] < counts[i-1][0] {
			t.Fatal("progress moved backwards")
		}
	}
}

func TestModuleProgressReporterDoesNotInventCompletions(t *testing.T) {
	var completed, total int
	p := newModuleProgressReporter(100, func(done, count int) {
		completed, total = done, count
	})
	p.completed.Add(12)
	p.finish()
	if completed != 12 || total != 100 {
		t.Fatalf("partial run reported %d/%d, want 12/100", completed, total)
	}
}
