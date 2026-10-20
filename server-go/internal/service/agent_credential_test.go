package service

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

func agentCredentialTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&repository.AgentAnthropicCredential{}); err != nil {
		t.Fatal(err)
	}
	return gdb
}

func seedAgentCredential(t *testing.T, gdb *gorm.DB, row repository.AgentAnthropicCredential) {
	t.Helper()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = db.NewFlexibleTime(time.Now())
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = db.NewFlexibleTime(time.Now())
	}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

func TestResolveActiveModelUsesPresetDefaultModel(t *testing.T) {
	gdb := agentCredentialTestDB(t)
	svc := NewAgentCredentialService(gdb)
	seedAgentCredential(t, gdb, repository.AgentAnthropicCredential{
		ID: 1, UserID: "u1", PresetID: "openai", SDKType: AgentSDKOpenAI,
		Protocol: "chat_completions", BaseURL: "https://relay.example/v1", APIKey: "sk-live",
		IsActive: true,
	})
	resolved, ok, err := svc.ResolveActiveModel(context.Background())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v，期望命中", ok, err)
	}
	if resolved.APIKey != "sk-live" || resolved.BaseURL != "https://relay.example/v1" {
		t.Fatalf("凭证字段漂移: %+v", resolved)
	}
	if resolved.Model != "gpt-5" {
		t.Fatalf("model 列为空应回落预设默认 gpt-5，得到 %q", resolved.Model)
	}
}

func TestResolveActiveModelPrefersCredentialModel(t *testing.T) {
	gdb := agentCredentialTestDB(t)
	svc := NewAgentCredentialService(gdb)
	model := "gpt-4.1"
	seedAgentCredential(t, gdb, repository.AgentAnthropicCredential{
		ID: 1, UserID: "u1", PresetID: "openai", SDKType: AgentSDKOpenAI,
		Protocol: "chat_completions", BaseURL: "https://relay.example/v1", APIKey: "sk-live",
		Model: &model, IsActive: true,
	})
	resolved, ok, err := svc.ResolveActiveModel(context.Background())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v，期望命中", ok, err)
	}
	if resolved.Model != "gpt-4.1" {
		t.Fatalf("model 列有值应优先，得到 %q", resolved.Model)
	}
}

func TestResolveActiveModelSkipsAnthropicProtocol(t *testing.T) {
	gdb := agentCredentialTestDB(t)
	svc := NewAgentCredentialService(gdb)
	seedAgentCredential(t, gdb, repository.AgentAnthropicCredential{
		ID: 1, UserID: "u1", PresetID: "__custom__", SDKType: AgentSDKClaude,
		Protocol: "anthropic_messages", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-x",
		IsActive: true,
	})
	_, ok, err := svc.ResolveActiveModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("anthropic_messages 协议不可驱动 Go 运行时，期望 ok=false")
	}
}

func TestResolveActiveModelIgnoresInactiveAndIncomplete(t *testing.T) {
	gdb := agentCredentialTestDB(t)
	svc := NewAgentCredentialService(gdb)
	// 非活跃行。
	seedAgentCredential(t, gdb, repository.AgentAnthropicCredential{
		ID: 1, UserID: "u1", PresetID: "openai", SDKType: AgentSDKOpenAI,
		Protocol: "chat_completions", BaseURL: "https://relay.example/v1", APIKey: "sk-idle",
		IsActive: false,
	})
	// 活跃但缺 api_key。
	seedAgentCredential(t, gdb, repository.AgentAnthropicCredential{
		ID: 2, UserID: "u1", PresetID: "openai", SDKType: AgentSDKOpenAI,
		Protocol: "chat_completions", BaseURL: "https://relay.example/v1", APIKey: " ",
		IsActive: true,
	})
	_, ok, err := svc.ResolveActiveModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("非活跃与缺密钥的行都应被跳过")
	}
}
