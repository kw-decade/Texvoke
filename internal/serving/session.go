package serving

// 会话级突破阶梯状态，两个部署形态共用。
//
// 为什么由服务端记而不是让接入方自己记：阶梯进度是「救援策略」的一部分，
// 散在各个反代里会出现三种事故——忘了记（每次都从 L1 骚扰）、记错（越界后
// 循环零执行导致空指针崩溃，2026-08-24 实测）、抄错（复制粘贴的状态机各自
// 漂移）。接入方只需要每个请求带同一个 session_key。
//
// 内存态：进程重启即清零。代价只是「下一个请求从 L1 重新说明一遍」，
// 不是安全或正确性问题，所以不需要持久化。

import "sync"

// MaxLadderLevel 是突破阶梯的最高级。L1 能力说明 → L2 运行时通知 →
// L3 完整示范 → L4 明示行动。超过它就该停手如实上报，而不是继续追问。
const MaxLadderLevel = 4

// maxTrackedSessions 防洪上限。键是摘要而非用户标识，清空的代价只是
// 个别长对话的阶梯从头再来一遍。
const maxTrackedSessions = 2000

// SessionState 是一个会话的救援事实快照。
type SessionState struct {
	// Level 是突破阶梯已用到的级数。0 表示全新或上次已成功。
	Level int
	// HandshakeDone 记录 L1 是否发过：同一会话只说明一次。
	HandshakeDone bool
	// HasCalls 记录历史里是否出现过工具调用。L2 用它反驳
	// 「调用接口不可用」的自我错觉。
	HasCalls bool
}

// SessionStore 是会话状态的并发安全容器。
//
// 所有变更都走带锁的方法，读出去的是值快照而不是指针——早先两份实现都把
// 裸指针交给调用方在锁外读写，同一个 session_key 上的并发请求会同时改
// Level。那是一个 race 测试抓不到的缺陷，因为没人写过「同键并发」的用例。
type SessionStore struct {
	mu sync.Mutex
	m  map[string]*SessionState
}

func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]*SessionState)}
}

// Advance 推进阶梯一级，返回本次该用的级数与推进后的事实快照。
// 返回的 level 大于 MaxLadderLevel 表示阶梯已用尽，调用方应停手。
func (s *SessionStore) Advance(key string) (int, SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.get(key)
	st.Level++
	return st.Level, *st
}

// MarkHandshake 记下「能力说明已经发过」。
func (s *SessionStore) MarkHandshake(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(key).HandshakeDone = true
}

// MarkCalls 记下「这个会话的历史里有成功的工具调用」。
func (s *SessionStore) MarkCalls(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(key).HasCalls = true
}

// Snapshot 只读地取出当前事实，不推进阶梯。键不存在时返回零值。
func (s *SessionStore) Snapshot(key string) SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.m[key]; ok {
		return *st
	}
	return SessionState{}
}

// Succeed 标记一次成功：调用已产出，阶梯归位。直接删除而不是归零——
// 下次卡住时从 L1 重新开始，与全新会话行为一致。
func (s *SessionStore) Succeed(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// get 取出或惰性创建状态。调用方必须已持锁。
//
// 防洪只在需要新建时才判：早先 sidecar 那份在每次 get 时无条件清空，
// 于是键空间一满，正在爬阶梯的活跃会话连 HandshakeDone 一起被抹掉，
// 表现是同一个对话被反复做能力说明。
func (s *SessionStore) get(key string) *SessionState {
	if st, ok := s.m[key]; ok {
		return st
	}
	if len(s.m) >= maxTrackedSessions {
		s.m = make(map[string]*SessionState)
	}
	st := &SessionState{}
	s.m[key] = st
	return st
}
