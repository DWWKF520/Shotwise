package providers

// 本文件是 builtin 后端的注册锚点：各供应商子包在各自 init() 里向 provider 注册表自注册，
// 拆分到子包后 init() 只有在被 import 时才会执行，这里必须空白导入全部 builtin 子包。
// 删掉任一条 = 对应供应商后端静默消失（编译期无任何报错，运行时表现为该供应商不可选）。
// 注：custom（声明式端点）相关代码就在本包，随本包一起加载，不需要空白导入。

import (
	_ "github.com/shotwise/shotwise/internal/providers/agnes"
	_ "github.com/shotwise/shotwise/internal/providers/ark"
	_ "github.com/shotwise/shotwise/internal/providers/dashscope"
	_ "github.com/shotwise/shotwise/internal/providers/gemini"
	_ "github.com/shotwise/shotwise/internal/providers/grok"
	_ "github.com/shotwise/shotwise/internal/providers/kling"
	_ "github.com/shotwise/shotwise/internal/providers/minimax"
	_ "github.com/shotwise/shotwise/internal/providers/newapi"
	_ "github.com/shotwise/shotwise/internal/providers/openai"
	_ "github.com/shotwise/shotwise/internal/providers/vidu"
)
