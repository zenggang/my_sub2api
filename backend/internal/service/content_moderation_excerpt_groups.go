package service

import (
	"fmt"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type ContentModerationExcerptGroup struct {
	Name     string  `json:"name"`
	MaxChars int     `json:"max_chars"`
	UserIDs  []int64 `json:"user_ids"`
}

type contentModerationExcerptRule struct {
	name     string
	maxChars int
}

func cloneContentModerationExcerptGroups(groups []ContentModerationExcerptGroup) []ContentModerationExcerptGroup {
	out := make([]ContentModerationExcerptGroup, len(groups))
	for i, group := range groups {
		out[i] = group
		out[i].UserIDs = append([]int64{}, group.UserIDs...)
	}
	return out
}

func normalizeContentModerationExcerptGroups(groups []ContentModerationExcerptGroup) ([]ContentModerationExcerptGroup, error) {
	out := cloneContentModerationExcerptGroups(groups)
	owners := make(map[int64]int)
	for i := range out {
		group := &out[i]
		group.Name = strings.TrimSpace(group.Name)
		if group.Name == "" {
			return nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_EXCERPT_GROUP", fmt.Sprintf("第 %d 个摘要分组名称不能为空", i+1))
		}
		if group.MaxChars < 1 || group.MaxChars > maxModerationInputRunes {
			return nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_EXCERPT_GROUP", fmt.Sprintf("摘要分组 %q 的字符上限必须是 1-%d 的整数", group.Name, maxModerationInputRunes))
		}
		ids := make([]int64, 0, len(group.UserIDs))
		seen := make(map[int64]struct{}, len(group.UserIDs))
		for _, userID := range group.UserIDs {
			if userID <= 0 {
				return nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_EXCERPT_GROUP", fmt.Sprintf("摘要分组 %q 的 UID 必须是正整数: %d", group.Name, userID))
			}
			if _, ok := seen[userID]; ok {
				continue
			}
			// 跨组重复直接拒绝，避免排序、保存顺序或热加载改变同一用户的生效长度。
			if owner, ok := owners[userID]; ok {
				return nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_EXCERPT_GROUP", fmt.Sprintf("UID %d 同时出现在摘要分组 %q（第 %d 组）和 %q（第 %d 组）", userID, out[owner].Name, owner+1, group.Name, i+1))
			}
			seen[userID] = struct{}{}
			owners[userID] = i
			ids = append(ids, userID)
		}
		group.UserIDs = ids
	}
	return out, nil
}

func newContentModerationExcerptRules(groups []ContentModerationExcerptGroup) map[int64]contentModerationExcerptRule {
	// 仅在已校验配置发布快照时构建，此后只读；请求路径不扫描名单或读数据库。
	rules := make(map[int64]contentModerationExcerptRule)
	for _, group := range groups {
		for _, userID := range group.UserIDs {
			rules[userID] = contentModerationExcerptRule{name: group.Name, maxChars: group.MaxChars}
		}
	}
	return rules
}

func (snapshot *contentModerationRuntimeSnapshot) excerptRule(userID int64) contentModerationExcerptRule {
	if snapshot != nil {
		if rule, ok := snapshot.excerptRules[userID]; ok {
			return rule
		}
	}
	return contentModerationExcerptRule{maxChars: maxModerationExcerptRunes}
}

func (input ContentModerationCheckInput) effectiveExcerptLimitRunes() int {
	if input.excerptLimitRunes > 0 && input.excerptLimitRunes <= maxModerationInputRunes {
		return input.excerptLimitRunes
	}
	// 兼容未经过 Check admission 的内部调用；不能从当前配置重新推断异步任务的上限。
	return maxModerationExcerptRunes
}
