import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('risk control locale copy', () => {
  it('describes worker runtime as audit and pre-block record processing', () => {
    expect(zh.admin.riskControl.workerStatusHint).toContain('前置拦截记录任务')
    expect(zh.admin.riskControl.workerStatusHint).not.toContain('异步观察任务')
    expect(en.admin.riskControl.workerStatusHint).toContain('pre-block record tasks')
    expect(en.admin.riskControl.workerStatusHint).not.toContain('observation tasks')
  })

  it('keeps pre-block audit key summary aware of async worker load', () => {
    expect(zh.admin.riskControl.preBlockAPIKeyLoadSummary).toContain('worker：{workerActive} / {workerTotal}')
    expect(en.admin.riskControl.preBlockAPIKeyLoadSummary).toContain('worker: {workerActive} / {workerTotal}')
  })

  it('does not describe pre-block audit key polling as bypassing the worker pool', () => {
    expect(zh.admin.riskControl.preBlockAPIKeyLoadHint).toBe('同步前置拦截直接轮询可用审核 Key。')
    expect(zh.admin.riskControl.preBlockAPIKeyLoadHint).not.toContain('Worker 池')
    expect(en.admin.riskControl.preBlockAPIKeyLoadHint).not.toContain('worker pool')
  })

  it('describes UID excerpt groups and uses their limit for Chinese filtering and retained summaries', () => {
    expect(zh.admin.riskControl.tabs.excerptGroups).toBe('摘要长度分组')
    expect(en.admin.riskControl.tabs.excerptGroups).toBe('Excerpt Groups')
    for (const messages of [zh, en]) {
      expect(messages.admin.riskControl.chineseOnlyHint).toContain('UID')
      expect(messages.admin.riskControl.chineseOnlyHint).not.toContain('500')
      expect(messages.admin.riskControl.recordNonHitsHint).toContain('UID')
      expect(messages.admin.riskControl.recordNonHitsHint).not.toContain('500')
      expect(messages.admin.riskControl.excerptGroups.duplicateUserID).toContain('{uid}')
      expect(messages.admin.riskControl.excerptGroups.duplicateUserID).toContain('{first}')
      expect(messages.admin.riskControl.excerptGroups.duplicateUserID).toContain('{second}')
    }
    expect(zh.admin.riskControl.excerptGroups.hint).toContain('无需重启')
    expect(en.admin.riskControl.excerptGroups.hint).toContain('without a restart')
  })
})
