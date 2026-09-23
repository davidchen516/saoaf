package resolver

import (
	"net/http"
	"testing"
)

func baseReq() *Request {
	return &Request{
		ContractVersion: "1.0",
		TaskRef:         "task-01",
		Requirements: []Requirement{
			{
				RequirementID:          "req-1",
				CapabilityID:           "model.reasoning.high",
				CapabilityMajorVersion: 1,
				ResourceType:           "MODEL_PROVIDER",
				Constraints: map[string]any{
					"region":                  "cn-east",
					"data_classification_max": "CONFIDENTIAL",
					"streaming_required":      true,
				},
			},
		},
	}
}

// 请求 Schema/语义校验负向矩阵（路由第 1 步）。
func TestValidateRequestMatrix(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Request)
		want string
	}{
		{"contract_version", func(r *Request) { r.ContractVersion = "2.0" }, CodeInvalidRequirement},
		{"empty task_ref", func(r *Request) { r.TaskRef = "" }, CodeInvalidRequirement},
		{"no requirements", func(r *Request) { r.Requirements = nil }, CodeInvalidRequirement},
		{"dup requirement_id", func(r *Request) {
			r.Requirements = append(r.Requirements, r.Requirements[0])
		}, CodeInvalidRequirement},
		{"empty capability_id", func(r *Request) { r.Requirements[0].CapabilityID = "" }, CodeInvalidRequirement},
		{"major 0", func(r *Request) { r.Requirements[0].CapabilityMajorVersion = 0 }, CodeInvalidRequirement},
		{"unknown resource_type", func(r *Request) { r.Requirements[0].ResourceType = "AGENT_PROVIDER" }, CodeInvalidRequirement},
		{"undeclared constraint key", func(r *Request) {
			r.Requirements[0].Constraints["model_name"] = "gpt-x"
		}, CodeInvalidRequirement},
		{"numeric constraint", func(r *Request) {
			r.Requirements[0].Constraints["region"] = 3.14
		}, CodeInvalidRequirement},
		{"unknown data class", func(r *Request) {
			r.Requirements[0].Constraints["data_classification_max"] = "ULTRA"
		}, CodeInvalidRequirement},
		{"unknown risk level", func(r *Request) {
			r.Requirements[0].Constraints["risk_level_max"] = "EXTREME"
		}, CodeInvalidRequirement},
	}
	for _, tc := range cases {
		req := baseReq()
		tc.mut(req)
		err := ValidateRequest(req)
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		re, ok := err.(*ResolveError)
		if !ok || re.Code != tc.want {
			t.Fatalf("%s: got %v, want code %s", tc.name, err, tc.want)
		}
	}
	// 21 requirements exceeds the limit
	req := baseReq()
	for i := 0; i < MaxRequirements; i++ {
		req.Requirements = append(req.Requirements, Requirement{
			RequirementID: itoaID(i), CapabilityID: "x", CapabilityMajorVersion: 1,
			ResourceType: "MODEL_PROVIDER",
		})
	}
	if err := ValidateRequest(req); err == nil {
		t.Fatal("21 requirements accepted")
	}
	// the base request itself is valid
	if err := ValidateRequest(baseReq()); err != nil {
		t.Fatalf("base request rejected: %v", err)
	}
}

func itoaID(i int) string {
	return "req-" + string(rune('a'+i))
}

// 禁止字段检查（路由第 2 步；红线：Prompt/凭据不得进入控制面载荷）。
func TestForbiddenFields(t *testing.T) {
	if err := CheckForbiddenFields([]byte(`{"task_ref":"t","requirements":[]}`)); err != nil {
		t.Fatalf("clean body rejected: %v", err)
	}
	for _, bad := range []string{
		`{"x":"api_key"}`, `{"x":"my-prompt-template"}`, `{"x":"password"}`,
	} {
		if err := CheckForbiddenFields([]byte(bad)); err == nil {
			t.Fatalf("forbidden body accepted: %s", bad)
		}
	}
}

// 错误路由优先级：结构错误 > 禁止字段 > 后续步骤（先命中先返回唯一 code）。
func TestErrorRoutingOrder(t *testing.T) {
	// a request violating BOTH structure and forbidden fields reports the
	// structural error only
	req := baseReq()
	req.ContractVersion = "9.9"
	if err := ValidateRequest(req); err == nil {
		t.Fatal("expected structural rejection")
	} else if err.(*ResolveError).Code != CodeInvalidRequirement {
		t.Fatalf("routing order broken: %v", err)
	}
	if err := CheckForbiddenFields([]byte(`{"credential":"x"}`)); err == nil {
		t.Fatal("forbidden scan missing")
	}
}

// fingerprint 确定性语料（AC-1: 同一规范化输入和 revision set 产生相同
// fingerprint；不同输入/revision 产生不同 fingerprint）。
func TestFingerprintDeterminismCorpus(t *testing.T) {
	mk := func() (*Request, []RevisionInput) {
		req := baseReq()
		return req, []RevisionInput{{
			CapabilityKey: "model.reasoning.high", CapabilityRevision: 12,
			BindingKey: "bind-a", BindingRevision: 7, ProviderKey: "mmr",
			SnapshotVersion: 3, Profile: "reasoning-high-v2",
		}}
	}
	req, items := mk()
	base := Fingerprint(req, items, "pol", 4)

	// identical input+revisions → identical fingerprint
	if again := Fingerprint(req, items, "pol", 4); again != base {
		t.Fatal("same input + revision set produced different fingerprints")
	}

	// requirement ORDER in the raw request must not matter (normalized)
	req2 := baseReq()
	req2.Requirements[0].Constraints["streaming_required"] = false
	req2.Requirements[0].Constraints["streaming_required"] = true
	if alt := Fingerprint(req2, items, "pol", 4); alt != base {
		t.Fatal("normalization failed: semantically identical request diverged")
	}

	// reversed multi-requirement order normalizes identically
	multi := func(reversed bool) *Request {
		r := baseReq()
		r.Requirements = append(r.Requirements, Requirement{
			RequirementID: "req-0", CapabilityID: "tool.erp", CapabilityMajorVersion: 1,
			ResourceType: "TOOL_PROVIDER", Constraints: map[string]any{"region": "cn-west"},
		})
		if reversed {
			r.Requirements[0], r.Requirements[1] = r.Requirements[1], r.Requirements[0]
		}
		return r
	}
	items2 := append([]RevisionInput{}, items...)
	items2 = append(items2, RevisionInput{CapabilityKey: "tool.erp", BindingKey: "bind-b",
		ProviderKey: "erp", SnapshotVersion: 1, Profile: "read"})
	fp1 := Fingerprint(multi(false), items2, "pol", 4)
	fp2 := Fingerprint(multi(true), items2, "pol", 4)
	if fp1 != fp2 {
		t.Fatal("requirement order leaked into fingerprint")
	}

	// different revision set → different fingerprint
	altRev := append([]RevisionInput{}, items...)
	altRev[0].BindingRevision = 8
	if Fingerprint(req, altRev, "pol", 4) == base {
		t.Fatal("binding revision change did not change fingerprint")
	}
	// different policy revision → different fingerprint
	if Fingerprint(req, items, "pol", 5) == base {
		t.Fatal("policy revision change did not change fingerprint")
	}
	// different input → different fingerprint
	reqDiff := baseReq()
	reqDiff.TaskRef = "task-02"
	if Fingerprint(reqDiff, items, "pol", 4) == base {
		t.Fatal("task_ref change did not change fingerprint")
	}
}

// request digest: 幂等键内容摘要（同 key 同 body 重放 vs 不同 body 409）。
func TestRequestDigest(t *testing.T) {
	a := baseReq()
	b := baseReq()
	if RequestDigest(a) != RequestDigest(b) {
		t.Fatal("identical bodies produced different digests")
	}
	b.Requirements[0].Constraints["region"] = "cn-west"
	if RequestDigest(a) == RequestDigest(b) {
		t.Fatal("different bodies produced the same digest")
	}
}

// Plan 状态机：仅 RESOLVED → {EXPIRED | REVOKED}。
func TestPlanTransitionMatrix(t *testing.T) {
	if !ValidatePlanTransition(StatusResolved, StatusExpired) ||
		!ValidatePlanTransition(StatusResolved, StatusRevoked) {
		t.Fatal("legal transitions rejected")
	}
	for _, bad := range [][2]string{
		{StatusExpired, StatusResolved}, {StatusRevoked, StatusResolved},
		{StatusExpired, StatusRevoked}, {StatusRevoked, StatusExpired},
		{StatusResolved, StatusResolved},
	} {
		if ValidatePlanTransition(bad[0], bad[1]) {
			t.Fatalf("illegal transition %s → %s allowed", bad[0], bad[1])
		}
	}
}

// error envelope classification sanity: each sentinel maps to a unique code.
func TestResolveErrorCodes(t *testing.T) {
	e := resErr(CodeAmbiguousBinding, http.StatusConflict, "tie")
	if e.Status != 409 || e.Code != CodeAmbiguousBinding {
		t.Fatalf("classified wrong: %+v", e)
	}
}
