package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestAllChecksPassedFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		check scm.Check
		ready bool
	}{
		{name: "pass", check: scm.Check{Bucket: scm.CheckBucketPass}, ready: true},
		{name: "skip", check: scm.Check{Bucket: scm.CheckBucketSkip}, ready: true},
		{name: "pending", check: scm.Check{Bucket: scm.CheckBucketPending}},
		{name: "failure", check: scm.Check{Bucket: scm.CheckBucketFail}},
		{name: "cancel", check: scm.Check{Bucket: scm.CheckBucketCancel}},
		{name: "unknown", check: scm.Check{}, ready: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := []scm.Check{tt.check}
			if got := allChecksPassed(checks); got != tt.ready {
				t.Fatalf("allChecksPassed() = %v, want %v", got, tt.ready)
			}
			if !tt.ready && !hasUnresolvedChecks(checks) && tt.check.Bucket != scm.CheckBucketFail {
				t.Fatal("non-ready check must be unresolved or failing")
			}
		})
	}
	if allChecksPassed(nil) {
		t.Fatal("empty checks must not pass")
	}
}

func TestPendingCheckMatchesLastFixed_SpecialCheckNames(t *testing.T) {
	t.Parallel()

	lastFixedChecks := encodeLastFixedChecks([]string{"lint,unit", "deploy+conflict"}, true)
	checks := []scm.Check{
		{Name: "lint,unit", Bucket: "pending"},
	}

	if !pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected pending check with special characters to match encoded last fixed checks %q", lastFixedChecks)
	}

	checks = []scm.Check{
		{Name: "lint", Bucket: "pending"},
	}
	if pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected unrelated pending check not to match encoded last fixed checks %q", lastFixedChecks)
	}
}

func TestEncodeLastFixedChecks_UsesStableSortedReviewCommentKeys(t *testing.T) {
	comments := []scm.ReviewComment{
		{ID: "comment-b", Author: "bot", Path: "b.go", Line: 2},
		{ID: "comment-a", Author: "bot", Path: "a.go", Line: 1},
	}
	first := encodeLastFixedChecks(nil, false, comments)
	second := encodeLastFixedChecks(nil, false, []scm.ReviewComment{comments[1], comments[0]})
	if first != second {
		t.Fatalf("reordered review comments changed fix key: %q != %q", first, second)
	}
	replaced := encodeLastFixedChecks(nil, false, []scm.ReviewComment{
		{ID: "comment-c", Author: "bot", Path: "b.go", Line: 2},
		comments[1],
	})
	if first == replaced {
		t.Fatalf("replaced review comment reused fix key: %q", first)
	}
}

func TestCIFailureOutcomeBoundsReviewFindings(t *testing.T) {
	t.Parallel()

	comments := make([]scm.ReviewComment, 64)
	for i := range comments {
		comments[i] = scm.ReviewComment{
			ID:     fmt.Sprintf("comment-%d", i),
			Author: "review-bot",
			Path:   "pkg/large.go",
			Line:   i + 1,
			Body:   strings.Repeat("x", maxCommentBodyBytes),
		}
	}

	outcome := ciFailureOutcome(nil, false, comments, "review findings")
	if len(outcome.Findings) > maxCIFindingsBytes {
		t.Fatalf("findings payload is %d bytes, want <= %d", len(outcome.Findings), maxCIFindingsBytes)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	var omitted bool
	for _, finding := range findings.Items {
		if finding.ID == "review-comments-omitted" {
			omitted = true
			if !strings.Contains(finding.Description, "additional unresolved PR review comments omitted") || !strings.Contains(finding.Description, "comment-") {
				t.Fatalf("omission finding lacks count and identifiers: %#v", finding)
			}
		}
	}
	if !omitted {
		t.Fatalf("expected oversized review findings to include an omission marker: %#v", findings.Items)
	}
}

func TestCIFailureOutcomeSanitizesReviewCommentTerminalControls(t *testing.T) {
	t.Parallel()

	comment := scm.ReviewComment{
		ID:     "terminal-control",
		Author: "review-bot",
		Path:   "pkg/foo.go",
		Line:   12,
		Body:   "before \x1b[31mred\x1b[0m \x1b]0;spoof\x07after\x07",
	}
	outcome := ciFailureOutcome(nil, false, []scm.ReviewComment{comment}, "review findings")
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %#v, want one finding", findings.Items)
	}
	description := findings.Items[0].Description
	if strings.ContainsAny(description, "\x1b\x07") || strings.Contains(description, "spoof") {
		t.Fatalf("terminal controls survived findings sanitization: %q", description)
	}
	if !strings.Contains(description, "before red after") {
		t.Fatalf("sanitization removed printable review content: %q", description)
	}
	prompt := formatReviewComments([]scm.ReviewComment{comment})
	if !strings.Contains(prompt, "\\u001b[31m") {
		t.Fatalf("review prompt did not retain JSON-framed raw comment content: %q", prompt)
	}
}

// A cancelled check can be a fix target, so the completion snapshot that lets
// the step notice its own CI re-run has to cover it. Keyed on the fail bucket
// alone, a cancelled-only fix round records nothing and the step can only log
// "fix already attempted" until its idle timeout.
func TestTerminalFailureCompletionTimesCoverCancelledChecks(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cancelled := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED", CompletedAt: completed}

	before := terminalFailureCompletionTimes([]scm.Check{cancelled})
	if got, ok := before["build"]; !ok || !got.Equal(completed) {
		t.Fatalf("completion times = %v, want the cancelled check recorded at %v", before, completed)
	}

	if terminalFailureCompletedAfter([]scm.Check{cancelled}, before) {
		t.Fatal("the same observation must not read as a re-run")
	}

	rerun := cancelled
	rerun.CompletedAt = completed.Add(2 * time.Minute)
	if !terminalFailureCompletedAfter([]scm.Check{rerun}, before) {
		t.Fatal("a cancelled check that completed again after the fix push must read as a re-run")
	}
}

// The fail bucket keeps the behavior it always had.
func TestTerminalFailureCompletionTimesStillCoverFailingChecks(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	failing := scm.Check{Name: "lint", Bucket: scm.CheckBucketFail, State: "FAILURE", CompletedAt: completed}

	before := terminalFailureCompletionTimes([]scm.Check{failing})
	if got, ok := before["lint"]; !ok || !got.Equal(completed) {
		t.Fatalf("completion times = %v, want the failing check recorded at %v", before, completed)
	}

	rerun := failing
	rerun.CompletedAt = completed.Add(time.Minute)
	if !terminalFailureCompletedAfter([]scm.Check{rerun}, before) {
		t.Fatal("a failing check that completed again after the fix push must read as a re-run")
	}

	// Passing and skipped checks are not failures and must stay out of the
	// snapshot, or an unrelated green check would reset the fix bookkeeping.
	quiet := terminalFailureCompletionTimes([]scm.Check{
		{Name: "docs", Bucket: scm.CheckBucketPass, State: "SUCCESS", CompletedAt: completed},
		{Name: "flaky", Bucket: scm.CheckBucketSkip, State: "SKIPPED", CompletedAt: completed},
	})
	if quiet != nil {
		t.Fatalf("completion times = %v, want nothing recorded for non-failures", quiet)
	}
}
