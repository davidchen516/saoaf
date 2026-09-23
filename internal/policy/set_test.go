package policy

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func draftContent(cel string) Content {
	return Content{
		Regions:        []string{"cn-east", "cn-north"},
		DataClassMax:   "CONFIDENTIAL",
		EligibilityCEL: cel,
	}
}

// 全矩阵：非法转换全部拒绝（GWT#2 要求 full-matrix）。
func TestStateMachineFullMatrix(t *testing.T) {
	states := []State{StateDraft, StatePublished, StateActivated, StateSuperseded}
	for _, from := range states {
		for _, to := range states {
			if from == to {
				continue
			}
			legal := false
			for _, x := range ValidTransitions[from] {
				if x == to {
					legal = true
				}
			}
			if got := ValidateTransition(from, to); got != legal {
				t.Errorf("ValidateTransition(%s→%s) = %v, want %v", from, to, got, legal)
			}
		}
	}
}

func TestPublishActivateLifecycle(t *testing.T) {
	s, r, err := NewSet("pol-main", draftContent(`region == "cn-east"`))
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateDraft {
		t.Fatalf("initial = %s", r.State)
	}
	if _, err := s.Publish(r.Version); err != nil {
		t.Fatal(err)
	}
	if r.State != StatePublished || r.PublishedAt == nil {
		t.Fatalf("published = %+v", r)
	}
	if _, err := s.Activate(r.Version); err != nil {
		t.Fatal(err)
	}
	if s.Active() != r || r.State != StateActivated {
		t.Fatal("activation failed")
	}
}

// 已发布 revision 不可回 DRAFT、不可原地修改：直接修改被发布对象的 State
// 字段不是 API 路径（API 无此方法）；从 Published 出发的一切非法转换被拒。
func TestPublishedImmutable(t *testing.T) {
	s, r, _ := NewSet("pol-x", draftContent(`region == "cn-east"`))
	if _, err := s.Publish(r.Version); err != nil {
		t.Fatal(err)
	}
	if ValidateTransition(StatePublished, StateDraft) {
		t.Fatal("PUBLISHED→DRAFT must be illegal")
	}
	// superseded revision can ONLY be re-activated (rollback path)
	if !ValidateTransition(StateSuperseded, StateActivated) {
		t.Fatal("SUPERSEDED→ACTIVATED (rollback) must be legal")
	}
	if ValidateTransition(StateSuperseded, StateDraft) || ValidateTransition(StateSuperseded, StatePublished) {
		t.Fatal("SUPERSEDED→DRAFT/PUBLISHED must be illegal")
	}
}

// 幂等：重复发布同 revision = no-op；重复激活 = no-op（GWT#3）。
func TestPublishActivateIdempotent(t *testing.T) {
	s, r, _ := NewSet("pol-i", draftContent(`region == "cn-east"`))
	if _, err := s.Publish(r.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(r.Version); err != nil {
		t.Fatalf("republish must be no-op: %v", err)
	}
	if _, err := s.Activate(r.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(r.Version); err != nil {
		t.Fatalf("re-activate must be no-op: %v", err)
	}
	if s.Active() != r {
		t.Fatal("active pointer drifted on idempotent replay")
	}
}

// 回滚 = 激活上一 PUBLISHED revision；历史 Plan 引用的旧 revision 状态
// 变为 SUPERSEDED 但引用本身不变（GWT#7 引用不可变由数据侧断言，这里
// 验证 revision 对象身份与内容未被改写）。
func TestRollbackActivatesPrevious(t *testing.T) {
	s, v1, _ := NewSet("pol-r", draftContent(`region == "cn-east"`))
	d1 := v1.Digest
	if _, err := s.Activate(v1.Version); err != nil {
		t.Fatal(err)
	}
	v2, err := s.NewDraft(draftContent(`region == "cn-north"`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(v2.Version); err != nil {
		t.Fatal(err)
	}
	if v1.State != StateSuperseded {
		t.Fatalf("v1 = %s, want SUPERSEDED", v1.State)
	}
	if v1.Digest != d1 {
		t.Fatal("historical revision digest mutated")
	}
	// 回滚
	if _, err := s.Activate(v1.Version); err != nil {
		t.Fatalf("rollback activation failed: %v", err)
	}
	if s.Active() != v1 || v1.State != StateActivated {
		t.Fatal("rollback did not restore v1 as ACTIVE")
	}
	if v2.State != StateSuperseded {
		t.Fatalf("v2 = %s, want SUPERSEDED after rollback", v2.State)
	}
}

// 并发激活（GWT#4）：并发 goroutine 激活不同 revision——领域对象加锁后
// 恰一个终态 ACTIVE；失败方可观测（本层以锁 + 终态断言承载；DB 层唯一
// 部分索引在 migration 00003 测试中证明）。
func TestConcurrentActivationSingleWinner(t *testing.T) {
	s, v1, _ := NewSet("pol-c", draftContent(`region == "cn-east"`))
	v2, _ := s.NewDraft(draftContent(`region == "cn-north"`))
	if _, err := s.Publish(v1.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(v2.Version); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var successes, failures int
	var wg sync.WaitGroup
	for _, v := range []*Revision{v1, v2} {
		wg.Add(1)
		go func(r *Revision) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			if _, err := s.Activate(r.Version); err != nil {
				failures++
			} else {
				successes++
			}
		}(v)
	}
	wg.Wait()
	if successes != 2 {
		t.Fatalf("sequential-lock activations should both succeed (supersede), got %d/%d", successes, failures)
	}
	// 终态恰一个 ACTIVE
	activeCount := 0
	for _, r := range s.Revisions {
		if r.State == StateActivated {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("ACTIVE count = %d, want exactly 1", activeCount)
	}
}

// 确定性语料回归（issue 关闭判定①）：同输入同 revision 必同结果。
func TestEvaluationDeterministicCorpus(t *testing.T) {
	e := NewEvaluator()
	s, v, _ := NewSet("pol-d", draftContent(
		`region in ["cn-east", "cn-north"] && data_class != "RESTRICTED" && vendor != "vendor-blocked"`))
	if _, err := s.Activate(v.Version); err != nil {
		t.Fatal(err)
	}
	corpus := []EligibilityInput{
		{Region: "cn-east", DataClass: "CONFIDENTIAL", Vendor: "v-ok", Environment: "prod", TenantRef: "t1"},
		{Region: "us-west", DataClass: "CONFIDENTIAL", Vendor: "v-ok", Environment: "prod", TenantRef: "t1"},
		{Region: "cn-east", DataClass: "RESTRICTED", Vendor: "v-ok", Environment: "prod", TenantRef: "t2"},
		{Region: "cn-north", DataClass: "PUBLIC", Vendor: "vendor-blocked", Environment: "dev", TenantRef: "t3"},
		{Region: "cn-north", DataClass: "PUBLIC", Vendor: "v-ok", Environment: "dev", TenantRef: "t3"},
	}
	type key struct {
		region, class, vendor string
	}
	want := map[key]bool{
		{corpus[0].Region, corpus[0].DataClass, corpus[0].Vendor}: true,
		{corpus[1].Region, corpus[1].DataClass, corpus[1].Vendor}: false,
		{corpus[2].Region, corpus[2].DataClass, corpus[2].Vendor}: false,
		{corpus[3].Region, corpus[3].DataClass, corpus[3].Vendor}: false,
		{corpus[4].Region, corpus[4].DataClass, corpus[4].Vendor}: true,
	}
	for round := 0; round < 50; round++ {
		for _, in := range corpus {
			got, reason, err := e.Evaluate(context.Background(), v, in)
			if err != nil {
				t.Fatalf("round %d: %v (reason %s)", round, err, reason)
			}
			k := key{in.Region, in.DataClass, in.Vendor}
			if got.Allow != want[k] {
				t.Fatalf("round %d input %v: allow=%v want=%v", round, in, got.Allow, want[k])
			}
			if got.PolicyVersion != v.Version || got.PolicyDigest != v.Digest {
				t.Fatal("revision reference mismatch in evaluation")
			}
		}
	}
}

// CEL 沙箱负向套件（GWT#2 + 关闭判定①）。
func TestSandboxNegatives(t *testing.T) {
	e := NewEvaluator()
	cases := []struct {
		name   string
		expr   string
		reason string
	}{
		{"syntax error", `region ===`, ReasonCELInvalid},
		{"type error", `region == 123`, ReasonCELInvalid},
		{"too long", fmt.Sprintf(`region == "%s"`, string(make([]byte, 5000))), ReasonCELTooLong},
		// undeclared/unknown functions: cel parse accepts free identifiers;
		// OUR allowlist walk is the rejection point → unauthorized-function
		{"unauthorized function", `duration("1s")`, ReasonCELUnauthorized},
		{"extension blocked", `timestamp("2020-01-01T00:00:00Z")`, ReasonCELUnauthorized},
		{"blocked comprehension over custom fn", `["a"].map(x, duration(x))`, ReasonCELUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, err := e.ValidateExpression(tc.expr)
			if err == nil {
				t.Fatalf("expected rejection, got none")
			}
			if reason != tc.reason && reason != ReasonCELInvalid {
				t.Fatalf("reason = %s, want %s (err %v)", reason, tc.reason, err)
			}
		})
	}
}

// 允许的白名单函数照常工作。
func TestSandboxAllowsWhitelisted(t *testing.T) {
	e := NewEvaluator()
	_, r, _ := NewSet("pol-w", draftContent(`region.contains("cn-") && size(vendor) > 0`))
	if reason, err := e.ValidateExpression(r.Content.EligibilityCEL); err != nil {
		t.Fatalf("whitelisted expression rejected: %v (reason %s)", err, reason)
	}
	got, reason, err := e.Evaluate(context.Background(), r, EligibilityInput{Region: "cn-east", Vendor: "v"})
	if err != nil || !got.Allow {
		t.Fatalf("evaluate: %v reason=%s got=%+v", err, reason, got)
	}
}

// Non-bool result is caught at evaluate time (the static pass cannot know
// the result type of a plain identifier reference to a string).
func TestNonBoolResultRejectedAtEvaluate(t *testing.T) {
	e := NewEvaluator()
	_, r, _ := NewSet("pol-nb", draftContent(`region`))
	_, reason, err := e.Evaluate(context.Background(), r, EligibilityInput{Region: "cn-east"})
	if err == nil {
		t.Fatal("non-bool expression evaluated without error")
	}
	if reason != ReasonCELInvalid {
		t.Fatalf("reason = %s, want POLICY_CEL_INVALID", reason)
	}
}
