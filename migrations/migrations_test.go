// Package migrations_test is the I04 persistence gate: every test runs
// against REAL PostgreSQL 18.6 (docker locally, service container in CI).
// The DSN comes from SAOAF_TEST_PG_DSN; tests skip with a clear message
// when neither the DSN nor a local docker postgres is available, so
// plain `go test ./...` stays green outside persistence contexts while
// scripts/pgtest.sh and the CI migrations job always exercise them.
//
// goose runs as a pinned external CLI (GOOSE_BIN, v3.28.0) so the heavy
// multi-dialect dependency tree never enters this module's graph.
package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dsn(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("SAOAF_TEST_PG_DSN"); v != "" {
		return v
	}
	t.Skipf("SAOAF_TEST_PG_DSN not set — run scripts/pgtest.sh or the CI migrations job for the real-PostgreSQL gate")
	return ""
}

func gooseBin(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("GOOSE_BIN"); v != "" {
		return v
	}
	if p, err := exec.LookPath("goose"); err == nil {
		return p
	}
	t.Skip("goose CLI not found (pin: go install github.com/pressly/goose/v3/cmd/goose@v3.28.0)")
	return ""
}

// tests run with cwd = the migrations package dir, so the SQL
// files are directly here.
func migrationsDir() string { return "." }

func gooseRun(t *testing.T, dsn, sub string, args ...string) string {
	t.Helper()
	bin := gooseBin(t)
	args = append([]string{"-dir", migrationsDir(), "postgres", dsn, sub}, args...)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("goose %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func tryGooseRun(dsn, sub string, args ...string) (string, error) {
	bin := os.Getenv("GOOSE_BIN")
	if bin == "" {
		if p, err := exec.LookPath("goose"); err == nil {
			bin = p
		} else {
			return "", errors.New("goose not found")
		}
	}
	args = append([]string{"-dir", migrationsDir(), "postgres", dsn, sub}, args...)
	out, err := exec.Command(bin, args...).CombinedOutput()
	return string(out), err
}

func queryVersion(t *testing.T, dsn string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	var v int64
	if err := conn.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&v); err != nil {
		t.Fatalf("version query: %v", err)
	}
	return v
}

func freshDB(t *testing.T, baseDSN string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("saoaf_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	return withDBName(baseDSN, name)
}

// withDBName rewrites the URL path segment to the given database name.
func withDBName(baseDSN, name string) string {
	u, err := url.Parse(baseDSN)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

func dropDB(t *testing.T, baseDSN, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
}

// atomicWrite inserts a change_record and its outbox_event in ONE
// transaction — the half-commit=0 invariant helper (issue acceptance).
func atomicWrite(t *testing.T, conn *pgx.Conn, tenant, entityKind, entityID, eventID string, rev int) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var changeID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
		VALUES ($1, 'test-actor', 'test-trace', $2, $3, 'CREATE')
		RETURNING id`, tenant, entityKind, entityID).Scan(&changeID)
	if err != nil {
		t.Fatalf("insert change_record: %v", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('test.topic', '{"k":"v"}', $1, $2, 'test-aggregate', $3, $4)`,
		changeID, eventID, entityID, rev)
	if err != nil {
		t.Fatalf("insert outbox_event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// ---------------------------------------------------------------------------
// GWT#1: 空库 + 上一发布版本库（两条路径）
// ---------------------------------------------------------------------------

func TestApplyFromEmptyDatabase(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	gooseRun(t, db, "up")
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("version = %d, want 4", v)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	atomicWrite(t, conn, "tenant-a", "capability", "cap-001", "evt-001", 1)
	var rows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM saoaf.outbox_event`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("outbox rows = %d err=%v, want 1", rows, err)
	}
}

func TestUpgradePathFromPreviousRelease(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	// v1 = 上一发布版本的库（只应用 00001）
	gooseRun(t, db, "up-to", "1")
	if v := queryVersion(t, db); v != 1 {
		t.Fatalf("v1 version = %d, want 1", v)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// 上一版本应用在 v1 形状上正常工作
	atomicWrite(t, conn, "tenant-a", "capability", "cap-002", "evt-002", 1)

	// 升级到最新（expand：纯加法——00002/00003）
	gooseRun(t, db, "up")
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("latest version = %d, want 4", v)
	}

	// 旧应用（不认识 published_seq）继续工作；expand 兼容性证明
	atomicWrite(t, conn, "tenant-a", "capability", "cap-003", "evt-003", 2)
	var nulls int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM saoaf.outbox_event WHERE published_seq IS NULL`).Scan(&nulls); err != nil || nulls != 2 {
		t.Fatalf("published_seq NULL rows = %d err=%v, want 2 (expand keeps old rows valid)", nulls, err)
	}
}

// ---------------------------------------------------------------------------
// GWT#3: 幂等重跑
// ---------------------------------------------------------------------------

func TestMigrationIdempotentRerun(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	gooseRun(t, db, "up")
	gooseRun(t, db, "up") // no-op
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("version after rerun = %d, want 4", v)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// 每个版本恰好一条记录；重跑不产生重复版本行（goose v3 的版本表无 is_current 列）
	var dup int64
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM (SELECT version_id FROM goose_db_version GROUP BY version_id HAVING count(*) > 1) d`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("duplicated version rows after rerun: %d", dup)
	}
}

// ---------------------------------------------------------------------------
// GWT#4: 并发迁移进程 — advisory lock 恰一个执行
// ---------------------------------------------------------------------------

func TestConcurrentRunnersSingleLock(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	// I04 review P1: run the REAL migration entry (tools/migrator, advisory
	// lock 781927001) twice concurrently against a fresh database; exactly
	// one must succeed, the other must exit with the lock error, and the
	// final schema must be fully applied.
	// Build the migrator binary in the TEST GOROUTINE — t.Fatalf inside a
	// spawned goroutine is undefined behavior and hangs the suite on hosts
	// without a prebuilt binary (CI: gitignored build outputs).
	bin := migratorBin(t)
	runMigrator := func() (string, int) {
		cmd := exec.Command(bin, "-dsn", db, "-dir", ".", "up")
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			code = 1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		return string(out), code
	}

	a := goRunAsync(t, func() (string, int) { return runMigrator() })
	b := goRunAsync(t, func() (string, int) { return runMigrator() })
	outA, codeA := a()
	outB, codeB := b()

	succeeded, locked := 0, 0
	for _, c := range []int{codeA, codeB} {
		switch c {
		case 0:
			succeeded++
		case 3:
			locked++
		default:
			t.Fatalf("unexpected migrator exit %d: %s / %s", c, outA, outB)
		}
	}
	if succeeded != 1 || locked != 1 {
		t.Fatalf("want exactly one success and one lock-rejection, got %d/%d:\n%s\n%s", succeeded, locked, outA, outB)
	}
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("version after concurrent migrators = %d, want 4", v)
	}

	// lock released after exit: a subsequent migrator run is a clean no-op
	out, code := runMigrator()
	if code != 0 {
		t.Fatalf("post-release rerun failed (%d): %s", code, out)
	}
}

func migratorBin(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	p := filepath.Join(wd, "..", "tools", "migrator", "migrator")
	// always rebuild: a stale on-disk binary must not shadow the current
	// source (cwd = migrations package dir)
	out, err := exec.Command("go", "build", "-o", p, filepath.Join("..", "tools", "migrator")).CombinedOutput()
	if err != nil {
		t.Fatalf("build migrator: %v\n%s", err, out)
	}
	return p
}

func goRunAsync(t *testing.T, fn func() (string, int)) func() (string, int) {
	t.Helper()
	type res struct {
		out  string
		code int
	}
	ch := make(chan res, 1)
	go func() {
		o, c := fn()
		ch <- res{o, c}
	}()
	return func() (string, int) {
		r := <-ch
		return r.out, r.code
	}
}

// ---------------------------------------------------------------------------
// GWT#5: 迁移中断 → 安全重试（确定性阻断：锁住目标表使 v2 ALTER 阻塞后 kill）
// ---------------------------------------------------------------------------

func TestInterruptedMigrationRecoversSafely(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	gooseRun(t, db, "up-to", "1") // v1 就位；v2 将 ALTER outbox_event

	ctx := context.Background()
	blocker, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)

	// 以 ACCESS EXCLUSIVE 锁住 outbox_event，v2 的 ALTER 必然阻塞
	blkTx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blkTx.Exec(ctx, `LOCK TABLE saoaf.outbox_event IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	// 启动 goose up（将阻塞在 v2），确认其仍在运行后 kill -9
	bin := gooseBin(t)
	cmd := exec.Command(bin, "-dir", migrationsDir(), "postgres", db, "up")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // 等其进入阻塞
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		_ = cmd.Process.Kill() // kill -9 等效：SIGKILL
	}
	_ = cmd.Wait()

	// 中断后：版本仍为 1，无 published_seq（无半变更）
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if v := queryVersion(t, db); v != 1 {
		t.Fatalf("version after kill = %d, want 1 (no half-applied migration)", v)
	}
	var colExists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='saoaf' AND table_name='outbox_event' AND column_name='published_seq')`).Scan(&colExists); err != nil {
		t.Fatal(err)
	}
	if colExists {
		t.Fatal("published_seq exists after killed migration — half-applied change leaked")
	}

	// 释放锁后安全重试 → 完整收敛
	_ = blkTx.Rollback(ctx)
	gooseRun(t, db, "up")
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("version after retry = %d, want 4", v)
	}
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='saoaf' AND table_name='outbox_event' AND column_name='published_seq')`).Scan(&colExists); err != nil {
		t.Fatal(err)
	}
	if !colExists {
		t.Fatal("published_seq missing after successful retry")
	}
}

// ---------------------------------------------------------------------------
// GWT#6: 权限分离 — 应用角色（无 DDL）不能跑迁移
// ---------------------------------------------------------------------------

func TestAppRoleCannotRunMigrations(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS saoaf_noddl`) // roles are cluster-scoped
	if _, err := admin.Exec(ctx,
		`CREATE ROLE saoaf_noddl LOGIN PASSWORD 'noddl-pass'`); err != nil {
		t.Fatal(err)
	}

	noddlDSN := withDBName(base, filepath.Base(db))
	nu, _ := url.Parse(noddlDSN)
	q := nu.Query()
	q.Set("user", "saoaf_noddl")
	q.Set("password", "noddl-pass")
	nu.RawQuery = q.Encode()
	noddlDSN = nu.String()
	out, err := tryGooseRun(noddlDSN, "up")
	if err == nil {
		t.Fatalf("migration as DDL-less role unexpectedly succeeded:\n%s", out)
	}
	if !containsAny(out, "permission denied", "ERROR") {
		t.Fatalf("expected permission-denied error, got:\n%s", out)
	}

	// 权限分离的正确形态：owner 迁移后，应用角色（00001 授权的 saoaf_app）可 DML
	gooseRun(t, db, "up")
	if _, err := admin.Exec(ctx, `ALTER ROLE saoaf_app PASSWORD 'app-pass'`); err != nil {
		t.Fatal(err)
	}
	appDSN := withDBName(base, filepath.Base(db))
	au, _ := url.Parse(appDSN)
	aq := au.Query()
	aq.Set("user", "saoaf_app")
	aq.Set("password", "app-pass")
	au.RawQuery = aq.Encode()
	app, err := pgx.Connect(ctx, au.String())
	if err != nil {
		t.Fatalf("app role connect: %v", err)
	}
	defer app.Close(ctx)
	if _, err := app.Exec(ctx, `SELECT 1 FROM saoaf.change_record LIMIT 1`); err != nil {
		t.Fatalf("app role DML should work post-migration: %v", err)
	}
	// INSERT is granted (write path); DELETE is not (audit history immutable)
	if _, err := app.Exec(ctx, `INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation) VALUES ('t','a','tr','k','i','CREATE')`); err != nil {
		t.Fatalf("app role INSERT should be granted: %v", err)
	}
	if _, err := app.Exec(ctx, `DELETE FROM saoaf.change_record WHERE true`); err == nil {
		t.Fatal("app role DELETE must be denied (audit immutability)")
	}
	// 应用角色不能建表（无 DDL）—— GWT#6 的另一半
	if _, err := app.Exec(ctx, `CREATE TABLE saoaf.app_should_not_create (id INT)`); err == nil {
		t.Fatal("app role unexpectedly created a table — DDL must be denied")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0) {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 数据不变量：数据库层强制（负向证据）+ 半提交 = 0
// ---------------------------------------------------------------------------

func TestConstraintEnforcement(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	gooseRun(t, db, "up")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// 基线写入
	atomicWrite(t, conn, "tenant-a", "capability", "cap-100", "evt-100", 1)

	// 唯一约束：event_id 重复 → 23505
	_, err = conn.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('t','{}', (SELECT id FROM saoaf.change_record LIMIT 1), 'evt-100', 'a', 'x', 2)`)
	if !isPgCode(err, "23505") {
		t.Fatalf("duplicate event_id: want 23505, got %v", err)
	}

	// CHECK：非法 status → 23514
	_, err = conn.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (status, topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('WEIRD', 't','{}', (SELECT id FROM saoaf.change_record LIMIT 1), 'evt-101', 'a', 'x', 1)`)
	if !isPgCode(err, "23514") {
		t.Fatalf("bad status: want 23514, got %v", err)
	}

	// CHECK：aggregate_revision = 0 → 23514
	_, err = conn.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('t','{}', (SELECT id FROM saoaf.change_record LIMIT 1), 'evt-102', 'a', 'x', 0)`)
	if !isPgCode(err, "23514") {
		t.Fatalf("revision 0: want 23514, got %v", err)
	}

	// FK：不存在的 change_record_id → 23503
	_, err = conn.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('t','{}', 999999999, 'evt-103', 'a', 'x', 1)`)
	if !isPgCode(err, "23503") {
		t.Fatalf("fk violation: want 23503, got %v", err)
	}

	// CHECK：tenant_ref 空 → 23514
	_, err = conn.Exec(ctx, `
		INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
		VALUES ('', 'a', 't', 'k', 'i', 'CREATE')`)
	if !isPgCode(err, "23514") {
		t.Fatalf("empty tenant: want 23514, got %v", err)
	}
}

func TestTransactionalAtomicityNoHalfCommit(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	gooseRun(t, db, "up")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// 显式回滚：两表都无残留
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = tx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
		VALUES ('t','a','tr','k','i','CREATE') RETURNING id`).Scan(&id)
	_, _ = tx.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('t','{}',$1,'evt-rb','a','x',1)`, id)
	_ = tx.Rollback(ctx)
	assertCount(t, conn, 0)

	// 连接中途死亡（事务未提交即断开）：两表都无残留 —— 半提交 = 0
	victim, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	dtx, err := victim.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = dtx.QueryRow(ctx, `
		INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
		VALUES ('t','a','tr','k','i','CREATE') RETURNING id`).Scan(&id)
	_, _ = dtx.Exec(ctx, `
		INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
		VALUES ('t','{}',$1,'evt-dc','a','x',1)`, id)
	// 杀死连接（等效崩溃），不提交
	if err := victim.Close(ctx); err != nil { // pgx Close 会回滚未决事务
		t.Logf("close: %v", err)
	}
	assertCount(t, conn, 0)
}

func assertCount(t *testing.T, conn *pgx.Conn, want int) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`SELECT count(*) FROM saoaf.change_record`,
		`SELECT count(*) FROM saoaf.outbox_event`,
	} {
		var n int
		if err := conn.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != want {
			t.Fatalf("%s = %d, want %d", q, n, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 连接池压测与事务隔离（并发事务下的约束行为）
// ---------------------------------------------------------------------------

// TestPoolConcurrentWritesInvariant: 20 goroutines share a 5-conn pool; every
// writer runs the atomic change_record+outbox_event transaction. Invariants
// after the storm: no half commits (counts equal), no duplicates (unique
// constraint enforced under concurrency), pool never leaks beyond its cap.
func TestPoolConcurrentWritesInvariant(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))
	gooseRun(t, db, "up")

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Release()
			tx, err := conn.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var id int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation)
				VALUES ($1, 'pool-actor', $2, 'cap', $3, 'CREATE') RETURNING id`,
				fmt.Sprintf("tenant-%d", n%3), fmt.Sprintf("tr-%d", n), fmt.Sprintf("cap-%03d", n)).Scan(&id); err != nil {
				errs <- err
				return
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
				VALUES ('pool.topic', '{}', $1, $2, 'cap', $3, 1)`,
				id, fmt.Sprintf("evt-pool-%03d", n), fmt.Sprintf("cap-%03d", n)); err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent writer failed: %v", err)
		}
	}

	// 半提交 = 0：两表计数必须一致且等于 writer 数
	var cr, ob int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM saoaf.change_record`).Scan(&cr); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM saoaf.outbox_event`).Scan(&ob); err != nil {
		t.Fatal(err)
	}
	if cr != writers || ob != writers {
		t.Fatalf("counts after storm: change_record=%d outbox=%d, want %d each (half-commit leak)", cr, ob, writers)
	}

	// 并发下的唯一性：重放相同 event_id 20 次并发 → 恰 1 成功 19 拒绝
	var okCount, rejectCount atomic.Int64
	var rwg sync.WaitGroup
	for i := 0; i < writers; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, `
				INSERT INTO saoaf.outbox_event (topic, payload, change_record_id, event_id, aggregate_kind, aggregate_id, aggregate_revision)
				VALUES ('pool.topic', '{}', (SELECT id FROM saoaf.change_record LIMIT 1), 'evt-dup-storm', 'cap', 'dup', 1)`); err == nil {
				okCount.Add(1)
			} else if isPgCode(err, "23505") {
				rejectCount.Add(1)
			}
		}()
	}
	rwg.Wait()
	if okCount.Load() != 1 || rejectCount.Load() != int64(writers-1) {
		t.Fatalf("unique under concurrency: ok=%d rejected=%d, want 1/%d", okCount.Load(), rejectCount.Load(), writers-1)
	}
}

// ---------------------------------------------------------------------------
// GWT#2: 违反约束的 DDL → 失败且库保持一致、可安全重试
// ---------------------------------------------------------------------------

func TestFailedMigrationStaysConsistent(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	gooseRun(t, db, "up") // v2 就位

	// 在临时目录里伪造一个失败的 00003（合法语句 + 必败语句）
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "00903_bad.sql"), []byte(
		`-- +goose Up
ALTER TABLE saoaf.outbox_event ADD COLUMN probe_col TEXT;
INSERT INTO nonexistent_table VALUES (1);
-- +goose Down
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "00904_good.sql"), []byte(
		`-- +goose Up
ALTER TABLE saoaf.outbox_event ADD COLUMN good_col TEXT;
-- +goose Down
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	// 失败迁移：version 停在 2，probe_col 不存在（事务性 DDL 回滚）
	bin := gooseBin(t)
	if out, err := exec.Command(bin, "-dir", tmpDir, "postgres", db, "up").CombinedOutput(); err == nil {
		t.Fatalf("bad migration unexpectedly passed:\n%s", out)
	}
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("version after failed migration = %d, want 4", v)
	}
	var col bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='saoaf' AND table_name='outbox_event' AND column_name='probe_col')`).Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col {
		t.Fatal("probe_col exists after failed migration — partial change leaked")
	}

	// 修复失败语句（重试语义：删掉坏迁移，保留 good）后收敛
	if err := os.Remove(filepath.Join(tmpDir, "00903_bad.sql")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "-dir", tmpDir, "postgres", db, "up").CombinedOutput(); err != nil {
		t.Fatalf("retry after fix failed: %v\n%s", err, out)
	}
	var good bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='saoaf' AND table_name='outbox_event' AND column_name='good_col')`).Scan(&good); err != nil {
		t.Fatal(err)
	}
	if !good {
		t.Fatal("good_col missing after successful retry")
	}
}

// Up→Down→Up round-trip: every migration's Down section must be executable
// (review R2 finding: DROP TRIGGER with schema qualifier broke goose down).
func TestMigrationUpDownUpRoundTrip(t *testing.T) {
	base := dsn(t)
	db := freshDB(t, base)
	defer dropDB(t, base, filepath.Base(db))

	// up → down → up
	gooseRun(t, db, "up")
	// goose down rolls back the most recent migration (v3 → v2), exercising
	// 00003's Down section (trigger/function/table drops)
	if out, err := tryGooseRun(db, "down"); err != nil {
		t.Fatalf("goose down failed (Down section broken): %v\n%s", err, out)
	}
	if v := queryVersion(t, db); v != 3 {
		t.Fatalf("version after down = %d, want 3", v)
	}
	gooseRun(t, db, "up")
	if v := queryVersion(t, db); v != 4 {
		t.Fatalf("version after up-down-up = %d, want 4", v)
	}
}
