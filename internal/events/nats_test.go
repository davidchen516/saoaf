package events

// NATS JetStream integration (I10): real publishes with server-side dedup
// and the message-platform outage → recovery drill. The four-window kill -9
// crash matrix lives in crash_test.go. Requires Docker (nats:2.14.7-alpine
// cached) and SAOAF_TEST_PG_DSN; each test owns a disposable server.

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func docker(args ...string) ([]byte, error) {
	return exec.Command("docker", args...).CombinedOutput()
}

func newJetStream(conn *nats.Conn) (jetstream.JetStream, error) {
	return jetstream.New(conn)
}

func jsConsumerConfig(subject string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{FilterSubject: subject}
}

// natsServer starts a disposable JetStream server; returns its URL. The
// stop func is exposed for the outage drill.
func natsServer(t *testing.T, port int) (url string, stop func(), start func()) {
	t.Helper()
	name := fmt.Sprintf("saoaf-nats-it-%d-%d", port, time.Now().UnixNano())
	if out, err := dockerRun(name, port); err != nil {
		t.Fatalf("nats run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = dockerKill(name) })
	waitNats(fmt.Sprintf("nats://127.0.0.1:%d", port), t)
	return fmt.Sprintf("nats://127.0.0.1:%d", port),
		func() { _ = dockerStop(name) },
		func() {
			_ = dockerStart(name)
			waitNats(fmt.Sprintf("nats://127.0.0.1:%d", port), t)
		}
}

func dockerRun(name string, port int) ([]byte, error) {
	return docker("run", "-d", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:4222", port),
		"nats:2.14.7-alpine", "-js", "-sd", "/data")
}

func dockerStop(name string) error  { _, err := docker("stop", name); return err }
func dockerStart(name string) error { _, err := docker("start", name); return err }
func dockerKill(name string) error  { _, err := docker("rm", "-f", name); return err }

func waitNats(url string, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := nats.Connect(url); err == nil {
			_ = c.Drain()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("nats did not become ready")
}

// streamMsgs counts delivered messages on a subject from the main stream.
func streamMsgs(t *testing.T, url, subject string) int {
	t.Helper()
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Drain()
	js, err := newJetStream(conn)
	if err != nil {
		t.Fatal(err)
	}
	s, err := js.Stream(context.Background(), "SAOAF_EVENTS")
	if err != nil {
		return 0 // stream not created yet
	}
	_ = s
	// drain the subject via an ephemeral pull consumer: Fetch returns when
	// the batch is full or the max-wait expires — never blocks forever
	cctx, err := s.CreateOrUpdateConsumer(context.Background(), jsConsumerConfig(subject))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := cctx.Fetch(256, jetstream.FetchMaxWait(250*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for msg := range batch.Messages() {
			got++
			_ = msg.Ack()
			n++
		}
		if got == 0 {
			break
		}
	}
	return n
}

// GWT#1 + 真实 JetStream：发布到达流；崩溃窗口重发被服务端 MsgID 去重吸收。
func TestNATSPublishAndServerDedup(t *testing.T) {
	url, _, _ := natsServer(t, 4281)
	ctx := context.Background()
	tr, err := NewNATSTransport(ctx, NATSConfig{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	row := mkRow(1, "binding:nb1:2", "binding.published", "nb1", 2)
	if err := tr.Publish(ctx, row.Envelope()); err != nil {
		t.Fatal(err)
	}
	// crash-window replay: same CloudEvent id republished → absorbed
	if err := tr.Publish(ctx, row.Envelope()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := streamMsgs(t, url, "saoaf.binding.published"); got != 1 {
		t.Fatalf("stream msgs = %d, want 1 (server dedup absorbs the replay)", got)
	}
	if !tr.Healthy(ctx) {
		t.Fatal("transport should be healthy")
	}
}

// 断连→恢复演练（issue 必须提交的证据：停机 → backlog 走不了 → 恢复 →
// backlog 自动清空且无丢失）。
func TestNATSOutageDrill(t *testing.T) {
	url, stop, start := natsServer(t, 4282)
	withDBE(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		tr, err := NewNATSTransport(ctx, NATSConfig{URL: url})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Close()
		w := NewOutboxWorker(WorkerConfig{
			Pool: pool, Transport: tr, WorkerID: "drill",
			BatchSize: 10, LeaseTTL: 2 * time.Second, RetryMax: 50,
			RetryBackoff: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond,
			Logger: discardLogger(),
		}, Hooks{})
		// phase 1: healthy — 3 events drain
		for i := 0; i < 3; i++ {
			seedOutbox(t, pool, fmt.Sprintf("ev-drill-a%d", i), "binding.published", fmt.Sprintf("ba%d", i))
		}
		if n, err := w.DrainOnce(ctx); err != nil || n != 3 {
			t.Fatalf("phase1 drain n=%d err=%v", n, err)
		}
		// phase 2: platform down — 3 more events cannot leave the outbox
		for i := 0; i < 3; i++ {
			seedOutbox(t, pool, fmt.Sprintf("ev-drill-b%d", i), "binding.published", fmt.Sprintf("bb%d", i))
		}
		stop()
		if n, err := w.DrainOnce(ctx); err != nil || n != 3 {
			t.Fatalf("outage drain n=%d err=%v", n, err)
		}
		if w.Stats().Published != 3 {
			t.Fatalf("outage published = %d, want 3 (nothing new)", w.Stats().Published)
		}
		var backlog int
		_ = pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.outbox_event WHERE status IN ('PENDING','PUBLISHING')`).Scan(&backlog)
		if backlog != 3 {
			t.Fatalf("outage backlog = %d, want 3 (发布走不出去)", backlog)
		}
		// phase 3: platform recovers — backlog clears automatically, no loss
		start()
		if _, err := pool.Exec(ctx, `UPDATE saoaf.outbox_event SET next_retry_at = NULL`); err != nil {
			t.Fatal(err)
		}
		drained := false
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			_, _ = w.DrainOnce(ctx)
			_ = pool.QueryRow(ctx,
				`SELECT count(*) FROM saoaf.outbox_event WHERE status IN ('PENDING','PUBLISHING')`).Scan(&backlog)
			if backlog == 0 {
				drained = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !drained {
			t.Fatalf("backlog did not clear after recovery: %d", backlog)
		}
		// no loss: all six events on the wire exactly once
		if got := streamMsgs(t, url, "saoaf.binding.published"); got != 6 {
			t.Fatalf("stream msgs = %d, want 6 (无丢失、无重复)", got)
		}
		if w.Stats().Published != 6 {
			t.Fatalf("published total = %d, want 6", w.Stats().Published)
		}
	})
}
