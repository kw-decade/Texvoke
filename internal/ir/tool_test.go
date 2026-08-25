package ir

import "testing"

// 零值必须无效，这是「默认拒绝」在类型层面的体现：忘记设置等于不合规，
// 而不是悄悄拿到一个默认值。
//
// 只剩两个枚举：风险等级 / 副作用 / 信任级别与调用生命周期状态机随执行层
// 一起删除，见 tool.go 与 call.go 的说明。
func TestZeroValueEnumsAreInvalid(t *testing.T) {
	if (CallSource("")).Valid() {
		t.Error("CallSource 零值不应有效")
	}
	// InputForm 是刻意的例外：零值就是 InputFormObject，绝大多数工具的形态。
	// 理由见 tool.go——让三个协议的解码器一行都不用改。
	if !(InputForm("")).Valid() {
		t.Error("InputForm 零值应当有效（等于 object 形态）")
	}
	if (InputForm("bogus")).Valid() {
		t.Error("未定义的 InputForm 不应有效")
	}
}

func TestToolIDValid(t *testing.T) {
	tests := []struct {
		name string
		id   ToolID
		want bool
	}{
		{"正常", ToolID{"fs", "read_file", "1"}, true},
		{"带语义化版本", ToolID{"fs", "read_file", "1.2.0"}, true},
		{"namespace 为空", ToolID{"", "read_file", "1"}, false},
		{"name 为空", ToolID{"fs", "", "1"}, false},
		{"version 为空", ToolID{"fs", "read_file", ""}, false},
		{"name 含斜杠", ToolID{"fs", "read/file", "1"}, false},
		{"name 含 @", ToolID{"fs", "read@file", "1"}, false},
		{"name 含空格", ToolID{"fs", "read file", "1"}, false},
		{"name 含换行", ToolID{"fs", "read\nfile", "1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Valid(); got != tc.want {
				t.Errorf("ToolID%+v.Valid() = %v，期望 %v", tc.id, got, tc.want)
			}
		})
	}
}

// ParseToolID 与 ToolSpec 的测试随它们本身一起删除（见 tool.go 的说明）。
// ToolID 的构造与校验仍然被上面的 TestToolIDValid 盯着——那是本项目真正
// 在用的部分：从客户端声明的工具名构造 client/<name>@declared。
