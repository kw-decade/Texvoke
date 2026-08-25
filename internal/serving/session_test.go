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

func TestSessionSucceedResets(t *testing.T) {
	s := NewSessionStore()
	const key = "sess-B"
	s.Advance(key)
	s.Advance(key)
	s.MarkHandshake(key)

	s.Succeed(key)

	if snap := s.Snapshot(key); snap.Level != 0 || snap.HandshakeDone {
		t.Fatalf("成功后应完全归位，得到 %+v", snap)
	}
	if got, _ := s.Advance(key); got != 1 {
		t.Fatalf("归位后应从 L1 重新开始，得到 L%d", got)
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
