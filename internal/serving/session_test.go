package serving

import (
	"strconv"
	"sync"
	"testing"
)

func TestSessionLadderAdvancesAndCaps(t *testing.T) {
	s := NewSessionStore()
	const key = "sess-A"

	for want := 1; want <= MaxLadderLevel; want++ {
		got, _ := s.Advance(key)
		if got != want {
			t.Fatalf("第 %d 次 Advance 返回 %d——阶梯没有逐级递进", want, got)
		}
	}
	if got, _ := s.Advance(key); got <= MaxLadderLevel {
		t.Fatalf("封顶后应返回超过 %d 的级数让调用方停手，得到 %d", MaxLadderLevel, got)
	}
}

// 成功后阶梯归位，但**已经说明过的事实必须留下**。
//
// 2026-09-01 真实 codex 长程实测：旧实现在这里 delete 整条状态，同一个对话
// 里 L1 能力说明出现了 4 次，每次都紧跟一次成功调用之后——模型早被说明过，
// 却要为此再付一次上游往返（实测 8-15 秒）。HasCalls 同样被抹掉，而它正是
// L2 反驳「调用接口不可用」的证据。
func TestSessionSucceedResetsLadderButKeepsFacts(t *testing.T) {
	s := NewSessionStore()
	const key = "sess-B"
	s.Advance(key)
	s.Advance(key)
	s.MarkHandshake(key)

	s.Succeed(key)

	snap := s.Snapshot(key)
	if snap.Level != 0 {
		t.Fatalf("阶梯应归位到 0，得到 %d", snap.Level)
	}
	if !snap.HandshakeDone {
		t.Fatal("HandshakeDone 被抹掉了：同一个会话会被反复做能力说明")
	}
	if !snap.HasCalls {
		t.Fatal("刚产出的调用本身就是「这个会话能调用工具」的证据，不该丢")
	}
	// 阶梯本身从头爬（下次卡住先给最轻的手段），但因为 HandshakeDone 还在，
	// capability 侧会跳过 L1 直接走 L2 运行时通知。
	if got, _ := s.Advance(key); got != 1 {
		t.Fatalf("归位后阶梯应从 1 重新开始，得到 L%d", got)
	}
}

// 防洪：键空间不会无限增长。被清掉的会话阶梯从 L1 重来，那只是慢，不是错。
//
// ponytail: 整表清空而不是 LRU。触发条件是同时存在 2000 个不同会话，单机
// 网关到不了；真要保住活跃会话，就给 SessionState 加最后访问时间、只清最旧
// 的一半。相比之下旧 sidecar 那份是在**每次** get 时清，活跃会话必被波及。
func TestSessionFloodGuardBoundsKeySpace(t *testing.T) {
	s := NewSessionStore()
	for i := 0; i < maxTrackedSessions*2; i++ {
		s.Advance("filler-" + strconv.Itoa(i))
	}
	if snap := s.Snapshot("filler-0"); snap.Level != 0 {
		t.Fatal("最早的键还在，防洪没生效——键空间会无限增长")
	}
	// 刚写入的键必须还在：清空之后要能立刻继续用。
	last := "filler-" + strconv.Itoa(maxTrackedSessions*2-1)
	if snap := s.Snapshot(last); snap.Level != 1 {
		t.Fatalf("清空后新建的键状态丢了：%+v", snap)
	}
}

// 同一个 session_key 上的并发请求不得竞争 Level。旧实现把裸指针交给调用方
// 在锁外读写，-race 抓不到只是因为没人写过同键并发的用例。
func TestSessionStoreConcurrentSameKey(t *testing.T) {
	s := NewSessionStore()
	const key = "sess-hot"
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Advance(key)
			s.MarkCalls(key)
			_ = s.Snapshot(key)
		}()
	}
	wg.Wait()
	if snap := s.Snapshot(key); snap.Level != 50 {
		t.Fatalf("50 次并发 Advance 后 Level = %d，期望 50——有更新丢失", snap.Level)
	}
}
