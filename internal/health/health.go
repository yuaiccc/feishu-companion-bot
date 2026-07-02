package health

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Checker struct {
	feishuHealthFn func(ctx context.Context) error
	deepseekFn     func(ctx context.Context) error
	memoryFn       func() int
}

func NewChecker(feishuFn func(ctx context.Context) error, deepseekFn func(ctx context.Context) error, memoryFn func() int) *Checker {
	return &Checker{
		feishuHealthFn: feishuFn,
		deepseekFn:     deepseekFn,
		memoryFn:       memoryFn,
	}
}

type Result struct {
	Checks []CheckItem
}

type CheckItem struct {
	Name   string
	Status string
	Detail string
}

func (r *Result) IsAllOK() bool {
	for _, c := range r.Checks {
		if c.Status != "✓" {
			return false
		}
	}
	return true
}

func (c *Checker) Check(ctx context.Context) Result {
	var items []CheckItem

	items = append(items, c.checkFeishu(ctx))
	items = append(items, c.checkDeepSeek(ctx))
	items = append(items, c.checkMemory())

	return Result{Checks: items}
}

func (c *Checker) checkFeishu(ctx context.Context) CheckItem {
	if c.feishuHealthFn == nil {
		return CheckItem{Name: "飞书连接", Status: "?", Detail: "未配置"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.feishuHealthFn(ctx); err != nil {
		return CheckItem{Name: "飞书连接", Status: "✗", Detail: err.Error()}
	}
	return CheckItem{Name: "飞书连接", Status: "✓", Detail: "正常"}
}

func (c *Checker) checkDeepSeek(ctx context.Context) CheckItem {
	if c.deepseekFn == nil {
		return CheckItem{Name: "DeepSeek", Status: "?", Detail: "未配置"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.deepseekFn(ctx); err != nil {
		return CheckItem{Name: "DeepSeek", Status: "✗", Detail: err.Error()}
	}
	return CheckItem{Name: "DeepSeek", Status: "✓", Detail: "正常"}
}

func (c *Checker) checkMemory() CheckItem {
	if c.memoryFn == nil {
		return CheckItem{Name: "记忆库", Status: "?", Detail: "未配置"}
	}
	count := c.memoryFn()
	return CheckItem{Name: "记忆库", Status: "✓", Detail: fmt.Sprintf("%d 条记忆", count)}
}

func (r Result) BuildCard() map[string]interface{} {
	lines := make([]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		lines = append(lines, fmt.Sprintf("- %s: %s %s", c.Name, c.Status, c.Detail))
	}

	template := "green"
	if !r.IsAllOK() {
		template = "red"
	}

	return map[string]interface{}{
		"msg_type": "interactive",
		"card": map[string]interface{}{
			"schema": "2.0",
			"header": map[string]interface{}{
				"title": map[string]string{"tag": "plain_text", "content": "服务健康检查"},
				"template": template,
			},
			"body": map[string]interface{}{
				"elements": []interface{}{
					map[string]interface{}{
						"tag":     "markdown",
						"content": strings.Join(lines, "\n"),
					},
				},
			},
		},
	}
}
