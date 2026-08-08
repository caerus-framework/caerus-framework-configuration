package cf_configuration

import (
	"errors"
	"fmt"
	"strings"

	cf "github.com/caerus-framework/caerus-framework"
)

// Compile-time assertion: configuration is a JobSource.
var _ cf.JobSource = (*Configuration)(nil)

// normalizeJobSpec validates a source's Job declaration at AddSource time
// (fail-fast wiring error): a job needs a Flag, tasks must be non-empty and
// unique. Returns the normalized spec (empty when no job is declared).
func normalizeJobSpec(job cf.JobSpec) (cf.JobSpec, error) {
	if job.Flag == "" && len(job.Tasks) == 0 {
		return cf.JobSpec{}, nil
	}
	if job.Flag == "" {
		return cf.JobSpec{}, errors.New("job Tasks requires a Flag (declare --<module>[.<instance>].job)")
	}
	if strings.HasPrefix(job.Flag, "-") {
		return cf.JobSpec{}, fmt.Errorf("job flag --%s must not start with a dash", job.Flag)
	}
	seen := make(map[string]bool)
	tasks := make([]string, 0, len(job.Tasks))
	for _, task := range job.Tasks {
		task = strings.TrimSpace(task)
		if task == "" {
			return cf.JobSpec{}, fmt.Errorf("job --%s: empty task in Tasks", job.Flag)
		}
		if seen[task] {
			return cf.JobSpec{}, fmt.Errorf("job --%s: duplicate task %q", job.Flag, task)
		}
		seen[task] = true
		tasks = append(tasks, task)
	}
	return cf.JobSpec{Flag: job.Flag, Tasks: tasks}, nil
}

// registerJob validates and stores a source's job declaration. dst receives the
// normalized spec; a job must name an Owner so JobRequests can route it.
func registerJob(name, owner string, job cf.JobSpec, dst *cf.JobSpec) error {
	if job.Flag == "" && len(job.Tasks) == 0 {
		return nil
	}
	if owner == "" {
		return fmt.Errorf("cf_configuration: source %q: job --%s requires an Owner (the job routes to the component the flag names)", name, job.Flag)
	}
	normalized, err := normalizeJobSpec(job)
	if err != nil {
		return fmt.Errorf("cf_configuration: source %q: %w", name, err)
	}
	*dst = normalized
	return nil
}

// JobRequests implements cf.JobSource. It inspects every registered source's
// declared job flag (the flag must have been parsed by ParseFlags) and reports
// the requested jobs: the flag names the instance (the source's Owner), the
// value names the task to run on it (e.g. --postgresql.job=migrate → run task
// "migrate" on the "postgresql" instance). A task outside the source's declared
// Tasks set is an error. CLI-only: file and environment values never produce a
// job request. Empty (no job flag provided) returns an empty slice.
//
// JobRequests must run after argv absorption; the framework calls it before any
// component initializes.
func (c *Configuration) JobRequests() ([]cf.JobRequest, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []cf.JobRequest
	seen := make(map[string]bool)
	for _, s := range c.sources {
		if s.job.Flag == "" {
			continue
		}
		raw, ok := c.flagValues[s.job.Flag]
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		task := strings.TrimSpace(raw)
		if len(s.job.Tasks) > 0 {
			supported := false
			for _, t := range s.job.Tasks {
				if t == task {
					supported = true
					break
				}
			}
			if !supported {
				return nil, fmt.Errorf("cf_configuration: job --%s: unknown task %q (supported: %s)", s.job.Flag, task, strings.Join(s.job.Tasks, ", "))
			}
		}
		if seen[s.owner] {
			continue
		}
		seen[s.owner] = true
		out = append(out, cf.JobRequest{Component: s.owner, Flag: s.job.Flag, Task: task})
	}
	return out, nil
}
