// Package repository 承载 GORM 模型与数据访问。
//
// 分层契约：domain ← repository ← service ← transport。repository 不得反向依赖上层。
//
// 表结构真相源是 migrations/0001_init.sql（从既有 projects/.shotwise.db 的实测 DDL 导出），
// 这里只做「列 ↔ 字段」映射，不用 AutoMigrate 反推 schema。
package repository

// AllModels 返回全部业务表的零值实例，供迁移校验与「读全表」一致性测试遍历。
func AllModels() []any {
	return []any{
		&AgentAnthropicCredential{},
		&AgentSessionEntry{},
		&AgentSessionEventLog{},
		&AgentSessionSummary{},
		&AgentSessionUserMessageLink{},
		&AgentSession{},
		&APICall{},
		&APIKey{},
		&AssemblyPlanRevision{},
		&AssemblyPlan{},
		&Asset{},
		&BudgetReservation{},
		&CreationCompatibilityEvent{},
		&CreationPlan{},
		&CreationSkillCompatibility{},
		&CreationSkillDefinition{},
		&CreationSkillVersion{},
		&CreativeBoardEdge{},
		&CreativeBoardItem{},
		&CreativeBoardVersion{},
		&CreativeBoard{},
		&CustomProvider{},
		&CustomProviderModel{},
		&ExternalExecution{},
		&GenerationBatch{},
		&MediaAsset{},
		&MediaBinding{},
		&MediaDerivation{},
		&MemoryCandidate{},
		&MemoryEntry{},
		&ProjectEventLog{},
		&ProviderConfig{},
		&ProviderCredential{},
		&PublishJob{},
		&PublishingAccount{},
		&RenderArtifact{},
		&RenderJob{},
		&RenderReviewSnapshot{},
		&SystemSetting{},
		&Task{},
		&User{},
		&WorkerLease{},
		&WorkflowApproval{},
		&WorkflowDefinition{},
		&WorkflowEdge{},
		&WorkflowMarketplaceReview{},
		&WorkflowNodeRunItem{},
		&WorkflowNodeRun{},
		&WorkflowNode{},
		&WorkflowRevision{},
		&WorkflowRun{},
		&WorkflowTemplate{},
		&WorkflowUsageStat{},
	}
}
