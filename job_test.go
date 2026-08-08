package cf_configuration

import (
	"strings"
	"testing"

	cf "github.com/caerus-framework/caerus-framework"
)

type jobSample struct {
	Name string `json:"name" env:"NAME"`
}

func newJobSource(t *testing.T, c *Configuration, name, owner string, job cf.JobSpec) {
	t.Helper()
	mustAdd(t, c, Source[jobSample]{Name: name, EnvPrefix: "JOB_", Owner: owner, Job: job})
}

func TestJobRequestsNoneByDefault(t *testing.T) {
	c := New()
	newJobSource(t, c, "db", "db", cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}})

	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("JobRequests = %+v, want none before ParseFlags", reqs)
	}
}

func TestJobRequestsFromFlagEqualsForm(t *testing.T) {
	c := New()
	newJobSource(t, c, "db", "db", cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}})

	if _, err := c.ParseFlags([]string{"--db.job=migrate"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("JobRequests = %+v, want one request", reqs)
	}
	if reqs[0].Component != "db" || reqs[0].Flag != "db.job" || reqs[0].Task != "migrate" {
		t.Fatalf("JobRequests = %+v, want {db db.job migrate}", reqs[0])
	}
}

func TestJobRequestsFromFlagSpaceForm(t *testing.T) {
	c := New()
	newJobSource(t, c, "db", "db", cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}})

	if _, err := c.ParseFlags([]string{"--db.job", "migrate"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Task != "migrate" {
		t.Fatalf("JobRequests = %+v, want the migrate task", reqs)
	}
}

func TestJobRequestsNamedInstance(t *testing.T) {
	c := New()
	newJobSource(t, c, "orders", "orders", cf.JobSpec{Flag: "postgresql.orders.job", Tasks: []string{"migrate"}})

	if _, err := c.ParseFlags([]string{"--postgresql.orders.job=migrate"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Component != "orders" || reqs[0].Flag != "postgresql.orders.job" {
		t.Fatalf("JobRequests = %+v, want the orders instance", reqs)
	}
}

func TestJobRequestsRejectsUnknownTask(t *testing.T) {
	c := New()
	newJobSource(t, c, "db", "db", cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}})

	if _, err := c.ParseFlags([]string{"--db.job=reindex"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	_, err := c.JobRequests()
	if err == nil || !strings.Contains(err.Error(), "reindex") {
		t.Fatalf("expected unknown-task error, got %v", err)
	}
}

func TestJobRequestsEmptyTasksAllowsAny(t *testing.T) {
	c := New()
	newJobSource(t, c, "db", "db", cf.JobSpec{Flag: "db.job"})

	if _, err := c.ParseFlags([]string{"--db.job=something"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Task != "something" {
		t.Fatalf("JobRequests = %+v, want the arbitrary task", reqs)
	}
}

func TestJobRequestsDeduplicatesSameOwner(t *testing.T) {
	c := New()
	newJobSource(t, c, "db", "db", cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}})
	newJobSource(t, c, "db2", "db", cf.JobSpec{Flag: "db2.job", Tasks: []string{"migrate"}})

	if _, err := c.ParseFlags([]string{"--db.job=migrate", "--db2.job=migrate"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Component != "db" {
		t.Fatalf("JobRequests = %+v, want the request deduplicated to one", reqs)
	}
}

func TestJobRequestsCliOnlyIgnoresEnvAndFile(t *testing.T) {
	c := New()
	t.Setenv("JOB_NAME", "env-junk")
	path := writeFile(t, t.TempDir(), "db.json", `{"name":"file-junk"}`)
	mustAdd(t, c, Source[jobSample]{
		Name: "db", Path: path, Format: FormatJSON, EnvPrefix: "JOB_", Owner: "db",
		Job: cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}},
	})

	// A config file "name" field and env JOB_NAME are loaded, but they are not
	// the job flag: the job request must come only from argv.
	reqs, err := c.JobRequests()
	if err != nil {
		t.Fatalf("JobRequests: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("JobRequests = %+v, want none (job is CLI-only)", reqs)
	}
}

func TestAddSourceRejectsBadJobDeclaration(t *testing.T) {
	c := New()
	path := writeFile(t, t.TempDir(), "db.json", `{}`)

	if err := AddSource(c, Source[jobSample]{Name: "db", Path: path, Format: FormatJSON, Job: cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate"}}}); err == nil {
		t.Fatal("job without an Owner should fail at AddSource")
	}
	if err := AddSource(c, Source[jobSample]{Name: "db", Path: path, Format: FormatJSON, Owner: "db", Job: cf.JobSpec{Tasks: []string{"migrate"}}}); err == nil {
		t.Fatal("Tasks without a Flag should fail at AddSource")
	}
	if err := AddSource(c, Source[jobSample]{Name: "db", Path: path, Format: FormatJSON, Owner: "db", Job: cf.JobSpec{Flag: "-db.job", Tasks: []string{"migrate"}}}); err == nil {
		t.Fatal("a dash-prefixed job flag should fail at AddSource")
	}
	if err := AddSource(c, Source[jobSample]{Name: "db", Path: path, Format: FormatJSON, Owner: "db", Job: cf.JobSpec{Flag: "db.job", Tasks: []string{"", "migrate"}}}); err == nil {
		t.Fatal("an empty task should fail at AddSource")
	}
	if err := AddSource(c, Source[jobSample]{Name: "db", Path: path, Format: FormatJSON, Owner: "db", Job: cf.JobSpec{Flag: "db.job", Tasks: []string{"migrate", "migrate"}}}); err == nil {
		t.Fatal("duplicate tasks should fail at AddSource")
	}
}

func TestJobFlagCollidesWithFieldFlag(t *testing.T) {
	c := New()
	path := writeFile(t, t.TempDir(), "db.json", `{}`)
	// flagSample's Host has flag:"host"; a job flag of the same name is a
	// wiring error caught at ParseFlags time.
	mustAdd(t, c, Source[flagSample]{Name: "db", Path: path, Format: FormatJSON, Owner: "db", Job: cf.JobSpec{Flag: "host", Tasks: []string{"migrate"}}})

	_, err := c.ParseFlags([]string{"--host=x"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with a field flag") {
		t.Fatalf("expected job/field flag collision error, got %v", err)
	}
}

func TestJobRequestsPropagatesNothingOnNil(t *testing.T) {
	var c *Configuration
	reqs, err := c.JobRequests()
	if err != nil || len(reqs) != 0 {
		t.Fatalf("nil JobRequests = %+v, %v; want empty, nil", reqs, err)
	}
}
