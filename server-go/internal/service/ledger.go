// 记账账本（ledger）：三通道封装 API 调用记账落库，对应 Python 侧 lib/ledger.py。
//
//  1. 记账括号：Start 落 pending 行并在块内暴露 call_id；成功显式调 FinishSuccess，
//     失败路径用 `defer ledger.FinishFailure(ctx, call, &err)` 兜底。
//  2. resume 补账：ResumeSuccess / ResumeFailed 按 call_id 精准翻 pending，幂等守卫
//     （WHERE status='pending'）保证不重复扣费。
//  3. 事后补录：Backfill 一次调用写入终态行（含 SDK 直报费用），无须调用方管理 pending。
//
// 结算落库后发一条 usage_record / recorded 项目变更（界面据此刷新用量）；无项目名的调用
// 不发，pending 阶段不发。
package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// 记账行状态与收口语义常量。
const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailed  = "failed"

	// interruptedCallMessage 是启动收口写入 error_message 的固定文案：中断的调用没有
	// 可用计费维度，不猜价。
	interruptedCallMessage = "interrupted_by_restart"

	usageRecordEntityType = "usage_record"
	usageRecordAction     = "recorded"
)

// ErrSuccessNotDeclared：生成路径正常返回但未显式声明成功。与 Python 侧记账括号正常退出
// 未调 call.success 抛 RuntimeError 同契约。
var ErrSuccessNotDeclared = errors.New("ledger: 生成路径返回时未声明成功")

// UsageRecordChange 是一次结算的项目变更条目。
//
// entity_type / action 与项目实体、任务终态都区分开；status 让消费方不必回查即可判断
// 这次结算是成功还是失败——service.Change 没有该字段，故结算事件自带结构。
type UsageRecordChange struct {
	EntityType string `json:"entity_type"`
	Action     string `json:"action"`
	EntityID   string `json:"entity_id"`
	Label      string `json:"label"`
	Focus      any    `json:"focus"`
	Important  bool   `json:"important"`
	Status     string `json:"status"`
}

// ChangePublisher 是结算事件的发布口。发布失败不影响账已落库的事实——事件只是实时性
// 优化，消费方有轮询兜底。
type ChangePublisher func(projectName string, changes []UsageRecordChange) error

// 构造一条 usage_record / recorded 变更。label 不进通知文案（important=false），仅作
// 日志与调试可读标识。
func newUsageRecordChange(callID int64, status string) UsageRecordChange {
	id := strconv.FormatInt(callID, 10)
	return UsageRecordChange{
		EntityType: usageRecordEntityType,
		Action:     usageRecordAction,
		EntityID:   id,
		Label:      id,
		Focus:      nil,
		Important:  false,
		Status:     status,
	}
}

// Ledger 承载三通道记账。同一实例可并发使用，但单个 Call 只应在同一生成路径内使用。
type Ledger struct {
	db      *gorm.DB
	calc    *pricing.Calculator
	publish ChangePublisher
}

// NewLedger 构造账本；calc 为费用计算器（nil 时仅显式费用/零费用结算可用）。
func NewLedger(gdb *gorm.DB, calc *pricing.Calculator) *Ledger {
	return &Ledger{db: gdb, calc: calc}
}

// SetPublisher 注入结算事件发布口；未注入时事件不发出（账照常落库）。
func (l *Ledger) SetPublisher(p ChangePublisher) { l.publish = p }

// StartParams 是 pending 行的落库参数，与 Python 侧 start_call 逐项对应。
type StartParams struct {
	ProjectName string
	CallType    string
	Model       string
	Prompt      string
	Resolution  string
	// DurationSeconds 为空写 NULL；请求口径在结算时被 backend 回报值覆盖。
	DurationSeconds *int
	AspectRatio     string
	// GenerateAudio 为 nil 按 Python 侧默认 True 落行。
	GenerateAudio *bool
	Provider      string
	UserID        string
	SegmentID     string
}

// Call 是记账括号句柄：块内即可用 ID（视频路径先持久化 call_id 再调 backend），
// 终局由 FinishSuccess / FinishFailed 显式声明。
type Call struct {
	ID int64

	projectName string
	finished    bool
}

// FinishParams 是一次终局结算的申报值。
type FinishParams struct {
	// Settlement 承载 provider 回报的计费维度（token/时长/显式费用等）。
	Settlement pricing.Settlement
	// OutputPath 成功时写入产物路径。
	OutputPath *string
	// Err 是失败原因，写入 error_message（截断 500）；成功路径忽略。
	Err error
	// CustomPrice 仅自定义供应商需要：调用方预先查库后传入，nil 时按无价结算。
	CustomPrice *pricing.CustomPrice
}

// Start 落一行 pending 记账并返回句柄。
func (l *Ledger) Start(ctx context.Context, p StartParams) (*Call, error) {
	now := time.Now().UTC()
	provider := p.Provider
	if provider == "" {
		provider = pricing.ProviderGemini
	}
	userID := p.UserID
	if userID == "" {
		userID = "default"
	}
	audio := p.GenerateAudio
	if audio == nil {
		audio = pricing.BoolPtr(true)
	}
	row := repository.APICall{
		ProjectName:     p.ProjectName,
		CallType:        p.CallType,
		Model:           p.Model,
		Prompt:          truncateText(p.Prompt, 500),
		Resolution:      optionalText(p.Resolution),
		DurationSeconds: int64PtrOf(p.DurationSeconds),
		AspectRatio:     optionalText(p.AspectRatio),
		GenerateAudio:   audio,
		Status:          StatusPending,
		StartedAt:       db.NewFlexibleTime(now),
		CreatedAt:       db.NewFlexibleTime(now),
		UpdatedAt:       db.NewFlexibleTime(now),
		Currency:        "USD",
		Provider:        provider,
		UserID:          userID,
		SegmentID:       optionalText(p.SegmentID),
	}
	if err := l.db.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return &Call{ID: row.ID, projectName: p.ProjectName}, nil
}

// FinishSuccess 成功结算：自动计费（显式 Settlement.CostAmount 优先）后翻 success。
//
// 同一 Call 的重复终局在这里不是幂等操作——生成路径只应声明一次；补账幂等由 resume
// 通道的 WHERE status='pending' 守卫承担。
func (l *Ledger) FinishSuccess(ctx context.Context, call *Call, p FinishParams) error {
	return l.finish(ctx, call, StatusSuccess, p)
}

// FinishFailed 失败结算：零费用（除非显式 Settlement.CostAmount）并写入错误信息。
func (l *Ledger) FinishFailed(ctx context.Context, call *Call, p FinishParams) error {
	return l.finish(ctx, call, StatusFailed, p)
}

// FinishFailure 是生成路径的失败兜底，替代 Python 侧记账括号的异常分支：
// 用 `defer ledger.FinishFailure(ctx, call, &err)` 声明，返回时若尚未显式声明终局，
// 未声明成功的路径必须留下 failed 记账（正常返回未声明成功时把错误写回调用方）。
// 取消（context.Canceled）穿透不记账，pending 行留待 resume 补账。
func (l *Ledger) FinishFailure(ctx context.Context, call *Call, errp *error) {
	if call == nil || call.finished {
		return
	}
	var cause error
	if errp != nil && *errp != nil {
		cause = *errp
		if errors.Is(cause, context.Canceled) {
			return
		}
	} else {
		cause = ErrSuccessNotDeclared
		if errp != nil {
			*errp = cause
		}
	}
	if err := l.FinishFailed(ctx, call, FinishParams{Err: cause}); err != nil {
		// 记账失败不吞原异常：仅记日志，原始错误照常冒泡。
		slog.Error("ledger 失败分支记账写入失败", "call_id", call.ID, "error", err)
	}
}

// BackfillParams 是事后补录的入参：用量与 SDK 直报费用由调用方显式给出。
type BackfillParams struct {
	ProjectName string
	CallType    string
	Model       string
	Provider    string
	Prompt      string
	UserID      string
	// Status 只接受 success / failed。
	Status string

	InputTokens  *int
	OutputTokens *int
	UsageTokens  *int
	CostAmount   *float64
	Currency     string
}

// Backfill 一次调用写入终态行，复用括号通道的结算口径，调用方无须管理 pending 中间态。
func (l *Ledger) Backfill(ctx context.Context, p BackfillParams) error {
	if p.Status != StatusSuccess && p.Status != StatusFailed {
		return errors.New("ledger: backfill status 只接受 success / failed")
	}
	call, err := l.Start(ctx, StartParams{
		ProjectName: p.ProjectName,
		CallType:    p.CallType,
		Model:       p.Model,
		Provider:    p.Provider,
		Prompt:      p.Prompt,
		UserID:      p.UserID,
	})
	if err != nil {
		return err
	}
	params := FinishParams{Settlement: pricing.Settlement{
		InputTokens:  p.InputTokens,
		OutputTokens: p.OutputTokens,
		UsageTokens:  p.UsageTokens,
		CostAmount:   p.CostAmount,
		Currency:     p.Currency,
	}}
	if p.Status == StatusSuccess {
		return l.FinishSuccess(ctx, call, params)
	}
	return l.FinishFailed(ctx, call, params)
}

// ResumeSuccess 成功补账：按 call_id 精准翻 pending → success，返回受影响行数（幂等 0/1）。
//
// resume 链路不新开括号（不重复 start），结算异常直接冒泡交调用方 finally 兜底。
func (l *Ledger) ResumeSuccess(ctx context.Context, apiCallID int64, s pricing.Settlement) (int64, error) {
	return l.resume(ctx, apiCallID, StatusSuccess, s)
}

// ResumeFailed 过期/失败补账：翻 pending → failed，显式零费用不重扣（幂等 0/1）。
// 错误信息不写行（原因沿调用链上抛由调用方兜底）。
func (l *Ledger) ResumeFailed(ctx context.Context, apiCallID int64) (int64, error) {
	return l.resume(ctx, apiCallID, StatusFailed, pricing.Settlement{CostAmount: pricing.Float64Ptr(0)})
}

// SettleInterrupted 是启动收口：把残留 pending 行翻 failed（零费用）并返回翻掉的行数。
//
// 经参数给定时只翻在该时刻之前发起的行（零值 time.Time 表示不设时间下界，调用方通常传
// 本进程构造时刻）；幂等由 UPDATE 的 WHERE status='pending' 承担，重复收口不覆写终态行。
func (l *Ledger) SettleInterrupted(ctx context.Context, startedBefore time.Time) (int, error) {
	var pendings []repository.APICall
	if err := l.db.WithContext(ctx).Where("status = ?", StatusPending).Find(&pendings).Error; err != nil {
		return 0, err
	}
	finishedAt := time.Now().UTC()
	flipped := 0
	type batch struct {
		project string
		changes []UsageRecordChange
	}
	var batches []batch
	batchIndex := map[string]int{}
	for i := range pendings {
		row := &pendings[i]
		if !startedBefore.IsZero() && row.StartedAt.Valid && !row.StartedAt.Time.Before(startedBefore) {
			continue
		}
		res := l.db.WithContext(ctx).Model(&repository.APICall{}).
			Where("id = ? AND status = ?", row.ID, StatusPending).
			Updates(map[string]any{
				"status":        StatusFailed,
				"finished_at":   db.NewFlexibleTime(finishedAt),
				"cost_amount":   0.0,
				"error_message": interruptedCallMessage,
				"updated_at":    db.NewFlexibleTime(finishedAt),
			})
		if res.Error != nil {
			return flipped, res.Error
		}
		if res.RowsAffected == 0 {
			continue
		}
		flipped++
		if row.ProjectName == "" {
			continue
		}
		change := newUsageRecordChange(row.ID, StatusFailed)
		if idx, ok := batchIndex[row.ProjectName]; ok {
			batches[idx].changes = append(batches[idx].changes, change)
			continue
		}
		batchIndex[row.ProjectName] = len(batches)
		batches = append(batches, batch{project: row.ProjectName, changes: []UsageRecordChange{change}})
	}
	for i := range batches {
		l.emit(batches[i].project, batches[i].changes)
	}
	return flipped, nil
}

// finish 是两条终态快照路径（括号 / backfill）共享的写入：结算后整行 UPDATE。
func (l *Ledger) finish(ctx context.Context, call *Call, status string, p FinishParams) error {
	if call == nil {
		return errors.New("ledger: nil call")
	}
	row, err := l.loadRow(ctx, call.ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 行已被级联删除：按 Python 侧语义静默跳过写入，事件照发。
		call.finished = true
		l.emit(call.projectName, []UsageRecordChange{newUsageRecordChange(call.ID, status)})
		return nil
	}
	if err != nil {
		return err
	}

	finishedAt := time.Now().UTC()
	settled, err := l.settle(row, finishedAt, p.Settlement, status == StatusSuccess, row.Currency, p.CustomPrice)
	if err != nil {
		return err
	}
	var errMsg *string
	if p.Err != nil {
		errMsg = truncateText(p.Err.Error(), 500)
	}

	res := l.db.WithContext(ctx).Model(&repository.APICall{}).
		Where("id = ?", call.ID).
		Updates(map[string]any{
			"status":              status,
			"finished_at":         db.NewFlexibleTime(finishedAt),
			"duration_ms":         settled.DurationMs,
			"duration_seconds":    settled.EffectiveDurationSeconds,
			"generate_audio":      settled.EffectiveGenerateAudio,
			"cost_amount":         settled.CostAmount,
			"currency":            settled.Currency,
			"usage_tokens":        p.Settlement.UsageTokens,
			"input_tokens":        settled.InputTokens,
			"output_tokens":       settled.OutputTokens,
			"image_input_tokens":  p.Settlement.ImageInputTokens,
			"image_output_tokens": p.Settlement.ImageOutputTokens,
			"text_input_tokens":   p.Settlement.TextInputTokens,
			"text_output_tokens":  p.Settlement.TextOutputTokens,
			"output_path":         p.OutputPath,
			"error_message":       errMsg,
			"updated_at":          db.NewFlexibleTime(finishedAt),
		})
	if res.Error != nil {
		return res.Error
	}
	call.finished = true
	l.emit(row.ProjectName, []UsageRecordChange{newUsageRecordChange(call.ID, status)})
	return nil
}

// resume 是 resume 两条通道共享实现：先读行快照，再按 WHERE status='pending' 守卫翻终态。
func (l *Ledger) resume(ctx context.Context, apiCallID int64, status string, s pricing.Settlement) (int64, error) {
	row, err := l.loadRow(ctx, apiCallID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	finishedAt := time.Now().UTC()
	baseCurrency := s.Currency
	if baseCurrency == "" {
		baseCurrency = "USD"
	}
	// autoCalc 只对「本次真的把 pending 翻 success」的结算生效；已终态行由 UPDATE 守卫兜住，
	// 计算阶段用行内状态判断即可。
	autoCalc := status == StatusSuccess && row.Status == StatusPending
	settled, err := l.settle(row, finishedAt, s, autoCalc, baseCurrency, nil)
	if err != nil {
		return 0, err
	}

	res := l.db.WithContext(ctx).Model(&repository.APICall{}).
		Where("id = ? AND status = ?", apiCallID, StatusPending).
		Updates(map[string]any{
			"status":           status,
			"finished_at":      db.NewFlexibleTime(finishedAt),
			"duration_ms":      settled.DurationMs,
			"duration_seconds": settled.EffectiveDurationSeconds,
			"cost_amount":      settled.CostAmount,
			"currency":         settled.Currency,
			"usage_tokens":     s.UsageTokens,
			"generate_audio":   settled.EffectiveGenerateAudio,
			"updated_at":       db.NewFlexibleTime(finishedAt),
		})
	if res.Error != nil {
		return 0, res.Error
	}
	affected := res.RowsAffected
	if affected > 0 {
		l.emit(row.ProjectName, []UsageRecordChange{newUsageRecordChange(apiCallID, status)})
	}
	return affected, nil
}

// loadRow 读单行快照。
func (l *Ledger) loadRow(ctx context.Context, id int64) (repository.APICall, error) {
	var row repository.APICall
	err := l.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	return row, err
}

// settle 把行快照与申报值交给计算器归一为生效结算值；baseCurrency 为空按 USD 兜底。
func (l *Ledger) settle(
	row repository.APICall,
	finishedAt time.Time,
	s pricing.Settlement,
	autoCalc bool,
	baseCurrency string,
	custom *pricing.CustomPrice,
) (pricing.SettledCall, error) {
	if baseCurrency == "" {
		baseCurrency = "USD"
	}
	return l.calc.Settle(pricing.SettleRow{
		CallType:        row.CallType,
		Model:           row.Model,
		Resolution:      derefText(row.Resolution),
		AspectRatio:     derefText(row.AspectRatio),
		DurationSeconds: intPtrOf(row.DurationSeconds),
		GenerateAudio:   row.GenerateAudio,
		Provider:        row.Provider,
		StartedAt:       row.StartedAt.Time,
	}, finishedAt, s, autoCalc, baseCurrency, custom)
}

// emit 发一条结算事件；项目名为空（端点试跑）不发。
func (l *Ledger) emit(projectName string, changes []UsageRecordChange) {
	if projectName == "" || len(changes) == 0 || l.publish == nil {
		return
	}
	if err := l.publish(projectName, changes); err != nil {
		// 账已落库，发布失败仅记日志。
		slog.Warn("ledger 结算事件发布失败", "project", projectName, "count", len(changes), "error", err)
	}
}

// truncateText 按字符（码点）截断到 max，空串归 nil——与 Python 侧切片语义一致。
func truncateText(s string, max int) *string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	if len(runes) > max {
		runes = runes[:max]
	}
	out := string(runes)
	return &out
}

// optionalText 空串写 NULL。
func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefText 读可选字符串，nil 按空串。
func derefText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// int64PtrOf 把可选 int 转成列类型 *int64。
func int64PtrOf(v *int) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}

// intPtrOf 把列类型 *int64 转回可选 int。
func intPtrOf(v *int64) *int {
	if v == nil {
		return nil
	}
	out := int(*v)
	return &out
}
