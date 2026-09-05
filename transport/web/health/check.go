package health

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Check executes all checks for kind concurrently. Each check receives its
// own timeout, panics are converted to down results, and the returned results
// are sorted by name independently of completion order.
func Check(ctx context.Context, kind Kind, checkers []NamedChecker, defaultTimeout time.Duration) Report {
	if !kind.valid() {
		return failedReport(kind, "health", fmt.Errorf("health: unsupported check kind %q", kind))
	}
	if ctx == nil {
		return failedReport(kind, "health", fmt.Errorf("health: check context cannot be nil"))
	}
	if defaultTimeout <= 0 {
		return failedReport(kind, "health", fmt.Errorf("health: default timeout must be greater than zero"))
	}

	selected := make([]NamedChecker, 0, len(checkers))
	for index, candidate := range checkers {
		candidateKind := candidate.Kind
		if candidateKind == "" {
			candidateKind = Readiness
		}
		if candidateKind != kind {
			continue
		}
		candidate.Kind = candidateKind
		if candidate.Name == "" {
			candidate.Name = fmt.Sprintf("check-%d", index+1)
		}
		selected = append(selected, candidate)
	}

	results := make([]Result, len(selected))
	done := make(chan struct{}, len(selected))
	for index := range selected {
		index := index
		go func() {
			results[index] = runOne(ctx, selected[index], defaultTimeout)
			done <- struct{}{}
		}()
	}
	for range selected {
		<-done
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Name == results[j].Name {
			return results[i].Kind < results[j].Kind
		}
		return results[i].Name < results[j].Name
	})

	report := Report{Kind: kind, Status: Up, Checks: results}
	for _, result := range results {
		if result.Status == Down {
			report.Status = Down
			break
		}
	}
	return report
}

func runOne(parent context.Context, item NamedChecker, defaultTimeout time.Duration) (result Result) {
	result = Result{Name: item.Name, Kind: item.Kind, Status: Down}
	started := time.Now()
	defer func() { result.Latency = time.Since(started) }()

	if item.Checker == nil {
		result.Error = fmt.Errorf("health: checker %q is nil", item.Name)
		return result
	}

	timeout := item.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 {
		result.Error = fmt.Errorf("health: checker %q has a negative timeout", item.Name)
		return result
	}
	if err := parent.Err(); err != nil {
		result.Error = err
		return result
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	completed := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("health: checker %q panicked: %v", item.Name, recovered)
			}
			completed <- err
		}()
		err = item.Checker.Check(ctx)
	}()

	select {
	case err := <-completed:
		result.Error = err
	case <-ctx.Done():
		// Prefer an already-completed result when completion and timeout become
		// observable together; this avoids turning a completed check into a
		// timeout solely because select chose the cancellation branch.
		select {
		case err := <-completed:
			result.Error = err
		default:
			if parent.Err() != nil {
				result.Error = parent.Err()
			} else {
				result.Error = fmt.Errorf("health: checker %q timed out after %s: %w", item.Name, timeout, context.DeadlineExceeded)
			}
		}
	}

	if result.Error == nil {
		result.Status = Up
	}
	return result
}

func failedReport(kind Kind, name string, err error) Report {
	return Report{
		Kind:   kind,
		Status: Down,
		Checks: []Result{{Name: name, Kind: kind, Status: Down, Error: err}},
	}
}
