package pipeline

import (
	"errors"
	"testing"
)

func TestNextStage(t *testing.T) {
	// Sprint 1 流水线：asr -> segment -> done（extract/quality/commit 在 Sprint 2 注入）
	flow := Flow{Stages: []string{"asr", "segment"}}
	if got := flow.Next("asr"); got != "segment" {
		t.Errorf("Next(asr) = %s", got)
	}
	if got := flow.Next("segment"); got != StageDone {
		t.Errorf("Next(segment) = %s", got)
	}
}

func TestApplySuccess(t *testing.T) {
	flow := Flow{Stages: []string{"asr", "segment"}}
	j := &JobState{Stage: "asr", Status: "running", Attempt: 2}
	if err := flow.Apply(j, nil); err != nil {
		t.Fatal(err)
	}
	if j.Stage != "segment" || j.Status != "pending" || j.Attempt != 0 {
		t.Fatalf("job = %+v", j)
	}
}

func TestApplyFailureRetryThenFail(t *testing.T) {
	flow := Flow{Stages: []string{"asr", "segment"}, MaxAttempt: 3}
	j := &JobState{Stage: "asr", Status: "running"}
	// 失败 1、2 次：回 pending，attempt 累加
	for wantAttempt := 1; wantAttempt <= 2; wantAttempt++ {
		if err := flow.Apply(j, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
		if j.Status != "pending" || j.Attempt != wantAttempt {
			t.Fatalf("attempt %d: job = %+v", wantAttempt, j)
		}
	}
	// 第 3 次失败：进 failed
	if err := flow.Apply(j, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if j.Status != "failed" || j.Stage != "asr" {
		t.Fatalf("final job = %+v", j)
	}
}

func TestApplyDone(t *testing.T) {
	flow := Flow{Stages: []string{"asr", "segment"}}
	j := &JobState{Stage: "segment", Status: "running"}
	if err := flow.Apply(j, nil); err != nil {
		t.Fatal(err)
	}
	if j.Status != "done" {
		t.Fatalf("job = %+v", j)
	}
}
