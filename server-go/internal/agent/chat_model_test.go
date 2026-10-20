package agent

import (
	"context"
	"errors"
	"testing"
)

// TestChatModelResolverConsultedPerCall 验证按次解析语义：每次 chatModel 调用都
// 走解析器（生产：prepareAgent 每轮回调 → 凭证切换从下一轮生效），不缓存实例。
func TestChatModelResolverConsultedPerCall(t *testing.T) {
	r, err := NewRuntime(context.Background(), ModelConfig{}, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.modelResolver = func(ctx context.Context) (ModelConfig, error) {
		calls++
		return ModelConfig{Model: "test-model", BaseURL: "http://127.0.0.1:9"}, nil
	}
	for i := 0; i < 2; i++ {
		m, err := r.chatModel(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if m == nil {
			t.Fatal("期望返回模型实例")
		}
	}
	if calls != 2 {
		t.Fatalf("解析器被调用 %d 次，期望按次共 2 次", calls)
	}
}

// TestChatModelResolverErrorPropagates 解析失败当场报错，不缓存、不回落启动期 cfg。
func TestChatModelResolverErrorPropagates(t *testing.T) {
	r, err := NewRuntime(context.Background(), ModelConfig{}, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.modelResolver = func(ctx context.Context) (ModelConfig, error) {
		return ModelConfig{}, errors.New("db down")
	}
	if _, err := r.chatModel(context.Background()); err == nil {
		t.Fatal("期望解析错误向上传播")
	}
}

// TestChatModelOverrideBeatsResolver override（测试注入）优先级高于解析器。
func TestChatModelOverrideBeatsResolver(t *testing.T) {
	r, err := NewRuntime(context.Background(), ModelConfig{}, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewChatModel(context.Background(), ModelConfig{Model: "test-model", BaseURL: "http://127.0.0.1:9"})
	if err != nil {
		t.Fatal(err)
	}
	r.chatModelOverride = m
	r.modelResolver = func(ctx context.Context) (ModelConfig, error) {
		t.Fatal("override 在场时解析器不应被调用")
		return ModelConfig{}, nil
	}
	got, err := r.chatModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Fatal("期望返回 override 注入的实例")
	}
}
