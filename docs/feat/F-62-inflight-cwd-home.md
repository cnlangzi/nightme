# F-62: In-flight 消息归属(CWD)— 修正跨 CWD 串扰与重复投递

> **Status**: 设计稿(讨论已收敛,未实现)
> **Date**: 2026-08-14
> **Author**: 夜me
> **Branch**: `fix-message-restore`(本仓在 worktree `fix-message-restore` 中开发)
> **触发**: 用户在飞书 `LzBook` 一个 chat 内先后 `/cwd` 到主仓与 `feat-dsh`,发现 `agent_sessions.json` 中两个 AS 的 `inFlightMessages` 都清不掉,主仓的 OneShotModel 消息被错投到 feat-dsh 进程,reply 贴卡错位。

---

## 1. 背景与动机

### 1.1 Incident 还原(2026-08-09 ~ 2026-08-14)

用户在 `cs_oc_09ef553acd586e2060a95cb5238e494c`(LzBook 飞书 chat)里先后跟两个 workspace 交互:

- `/cwd /Users/geax/.../cnlangzi/nightme` (主仓)
- `/cwd /Users/geax/.../nightme.nightme/feat-dsh` (feat-dsh worktree)

`/Users/geax/.nightme/agent_sessions.json` 在 08-14 的状态:

```json
"as_1786263862137778000_2": {
  "chatSessionId": "cs_oc_09ef553acd586e2060a95cb5238e494c",
  "cwd": "/Users/geax/code/geax/github.com/cnlangzi/nightme",
  "status": "detached",
  "suspectReason": "hung_prompt",
  "suspectSince": "2026-08-14T08:31:54+08:00",
  "inFlightMessages": [{
    "id": "om_x100b68d242058ca4b349af8c49325a6",
    "blocks": [{"Text": "先撤销刚才 OneShotModel的修改"}],
    "receivedAt": "2026-08-14T08:26:52.585+08:00"
  }]
},
"as_1786700564993704000_2": {
  "chatSessionId": "cs_oc_09ef553acd586e2060a95cb5238e494c",
  "cwd": "/Users/geax/code/geax/github.com/cnlangzi/nightme.nightme/feat-dsh",
  "status": "detached",
  "inFlightMessages": [{
    "id": "om_x100b68dad6cb10a8b341927c7ef067b",
    "blocks": [{"Text": "检测你的工作目录"}],
    "receivedAt": "2026-08-14T18:10:08.53+08:00"
  }]
}
```

用户观察到的两个症状:

1. **投递对象错了**: 主仓已经 hung 的 OneShotModel 消息,在 `/cwd feat-dsh` 后被新发消息"一起"塞给 feat-dsh 的 claude 进程,process 替主仓答了"无可撤销 / git tree clean"。
2. **投递完了之后已经执行了,为什么没清?**: 已经有 receipt card 显示 ✅ DONE 的消息(例如"OneShotModel 撤销"),in-flight 也仍然写着那条。下一条"检测你的工作目录" 进了新 batch,agent 还没 EventAgentDone,在 in-flight 里卡住。

### 1.2 根因(一句话)

**`cs.queue` 是 chat 级共享结构,但 message 的归属是 `(agent, cwd)`。**

`Manager.RestoreFromRegistry`(`internal/chatsession/manager.go:605-626`)在 daemon 重启时把每个 AS 持久化的 `InFlightMessages` **推回 `cs.queue`**,这一推丢失了归属信息:

- `cs.queue` 不知道队列里的消息"是哪个 AS 的"
- `TryFlush` 选的是 `selectedAgentSession`(按 `selectedAgent` + `selectedCwd`),跟 queue 里的消息本属于哪个 AS 无关
- 用户 `/cwd` 之后,旧 (agent, cwd) 的 in-flight 跟着新消息一起被推到新 AS 的 batch 里

附带衍生问题:

- `buildPromptLocked`(`chatsession.go:1087-1117`)用 `messages[n-1].ID` 作为 `LastMessageID`,把整批事件的 `UserMsgID` 锚到 batch 最后一条 → reply 贴卡错位
- `endPrompt`(`readpump.go:209-258`)是 in-flight 唯一的清除点,只在 `EventAgentDone`/`EventAgentError`/events channel 关闭时触发;agent hang 不发 EventAgentDone → in-flight 永远不会清

---

## 2. 目标 / 非目标

### 目标

- **in-flight 的归属明确为 `(agent, cwd)`**(chat_id 与 AS_id 都不是归属 key)
- **切 cwd 时旧 (agent, cwd) 的 in-flight 立即作废**(内存 + 磁盘),不留进新 queue
- **daemon 重启时不跨 AS 把 in-flight 推到 cs.queue**——只让 AS 内存里记着,从 queue 走真实新消息
- **修复后从同一 chat 模拟旧 incident 复现,确认两个症状都消失**

### 非目标

- 不动 `Message`/`Prompt`/`EventAgentDone` 的语义源头(F-53)
- 不重写 `cs.queue` 的内部结构(linked list / in-flight region),`Peek`/`Push`/`Commit` 行为不变
- 不在本次引入 `MessageRemoved` 通知(receipt card update 链是 EventAgentText → receipt adapter,不走 in-flight)
- 不动 HungPrompt → endPrompt 的修(F-61 已为它预留 `PromptEndStalledKilled`,独立 PR)
- 不动 prober 的真实 respawn 实现(F-61 留的 TODO,独立 PR)

---

## 3. 设计

### 3.1 三层身份的定义

| 维度 | 标识 | 作用 | 跟 in-flight 的关系 |
|------|------|------|---------------------|
| **chatID** | `cs_oc_...` | IM 通道,1:1 永久稳定 | ❌ 只是 IM 通道 |
| **AS_id** | `as_<ts>_<seq>` | 进程句柄,进程死一次就重建 | ❌ 运行时 handle |
| **`(agent, cwd)`** | `claude@/path/...` | agent 进程的"家" | ✅ **In-flight 的归属** |

> AS_id 当前实现等价于 `hash(chatID, agent, cwd)`,所以挂在 AS 上不会丢归属;问题是 `cs.queue` 不带这个 hash,把所有 AS 的 in-flight 抹平了。

### 3.2 当前链路的错位

```
用户消息 ──→ cs.queue.Push(msg)         (chat 级,key=chatID)
                │
                ▼
       TryFlush → Peek → batch
                │
                ▼
       Submit(batch, AS)                 (AS = 当前 selected agent+cwd)
                │
                ▼
       AS.inFlightMessages = batch       (per AS,key=AS_id)
                │
                ▼
       Agent 进程发回 EventAgentText/EventAgentDone
                │
                ▼
       endPrompt ─→ AS.inFlightMessages = nil   (per AS)
```

**链路上 AS.inFlightMessages 是对的,cs.queue 是错的。** cs.queue 应该只承担"当前 selected (agent, cwd) 的待投递消息",不应该被 restore 把任意 AS 的历史 in-flight 灌进来。

### 3.3 修复点(四处,原子完成)

#### 3.3.1 A. `RestoreFromRegistry` —— 不再 push in-flight 到 cs.queue

`internal/chatsession/manager.go:605-626` 整段 re-push 循环 + 注释删除。

`FromAgentSessionEntry`(`internal/agentsession/session.go:357-393`)继续从盘 load `InFlightMessages` 给 AS 内存用,**但 reload 后永远不进 cs.queue**。如果某条 AS 之后被选中、Submit 触发,Submit 会把内存里那批当作新 batch 覆盖掉;如果 AS 不再被选中,内存里的 in-flight 由下面 3.3.2 / 3.3.3 主动清掉。

#### 3.3.2 B. `SetSelectedCwd` —— 切 cwd 之前清旧 AS 的 in-flight

`internal/chatsession/chatsession.go:445-469` 在 `cs.selectedCwd = cwd` 赋值之前:

```go
// F-62: 旧 (agent, cwd) 的 in-flight 失联,主动清空并 persist
if cs.selectedCwd != "" && cs.selectedCwd != cwd {
    oldKey := agentCwdKey{Agent: cs.selectedAgent, Cwd: cs.selectedCwd}
    if oldAS, ok := cs.pool[oldKey]; ok {
        oldAS.ClearInFlight()
    }
}
```

语义:**用户主动切焦点的那一帧,旧 AS 的 in-flight 立刻作废。** 这是 chat-session 层(不是 AS 层)的"新 session" 边界。

#### 3.3.3 C. `SetSelectedAgent` —— 切 agent 之前清旧 AS 的 in-flight

`/use`(`internal/chatsession/chatsession.go:544`)只改 `cs.selectedAgent`,**不改 cwd**。因为 pool 的 key 是 `(agent, cwd)`,`/use` 之前的 key 跟之后 LookupSelectedAgentSession 用的 key 不可能命中,因此 hadPrior 分支(`/use` 之前那条 AS 永远不会被 hadPrior 覆盖)无法覆盖 `/use` 这条路径。修法跟 `SetSelectedCwd` 对齐:在 `cs.selectedAgent = agent` 赋值之前抓旧 AS 指针,释放 `cs.mu` 后调 `ClearInFlight`。

#### 3.3.3b D. `LookupSelectedAgentSession` hadPrior 分支 —— 同 (agent, cwd) 复用时清 in-flight

`internal/chatsession/chatsession.go:1569` 在 `hadPrior=true` 时(也就是同一个 `(agent, cwd)` 池里有一条 Detached/Exited 的旧 AS,需要 spawn 新进程),`existingAS` 拿到的是旧 AS 对象复用(变量名带"new"是因为这条路径会触发新 spawn,实际身份复用——见 §3.3.3b 的命名说明)。调用 `existingAS.ClearInFlight()` 把 in-flight 镜像扔掉,让 spawn 出来的新进程不继承上一轮的 stale 消息列表。

**C 与 D 的关系**:`/use` 切 agent 走 C,同 (agent, cwd) 重走 spawn 走 D;**两条路径相互独立,不能合并**。

#### 3.3.4 E. 新增 `AgentSession.ClearInFlight()`

`internal/agentsession/session.go` 新增方法,与 `endPrompt` 里的清空逻辑保持对称:

```go
// F-62: 旧 AS 失联或切 cwd 时调用,内存 + 磁盘同步清空 in-flight。
// 与 endPrompt(reason) 区别:不走事件链,不发 KindPromptEnded。
func (as *AgentSession) ClearInFlight() {
    as.asMu.Lock()
    if len(as.inFlightMessages) == 0 {
        as.asMu.Unlock()
        return
    }
    as.inFlightMessages = nil
    persist := as.persist
    as.asMu.Unlock()

    if persist != nil {
        if err := persist(as.Entry()); err != nil {
            slog.Warn("agentsession: persist after ClearInFlight failed",
                "as_id", as.ID, "err", err)
        }
    }
}
```

`as.persist` 已经在 `internal/chatsession/chatsession.go:1456`(`Wire the per-AS persist callback`)挂好,直接复用。

### 3.4 修复后场景再跑一遍

```
T0   daemon restart
T1   FromAgentSessionEntry:
       AS-主仓.inFlight = [OneShotModel]      (内存)
       AS-feat-dsh.inFlight = [检测你的工作目录] (内存)
       ※ cs.queue 不动
T2   用户 /cwd feat-dsh:
       SetSelectedCwd → 旧 AS-主仓.ClearInFlight()
       → AS-主仓.inFlight = nil,磁盘同步写空
T3   用户发 "OneShotModel 撤销":
       cs.queue = [OneShotModel 撤销]         (干净,只有新消息)
T4   TryFlush → AS-feat-dsh 提交
       → batch 里只剩新消息,LastMessageID = "OneShotModel 撤销"
       → 锚点正确,reply 贴卡正确
T5   后续 "检测你的工作目录" 单独提交
       → 单独 batch,锚点 = 自己
```

Bug 1 解决: 主仓的消息不再会被推到 feat-dsh 的 batch。
Bug 2 解耦: in-flight 在 /cwd 切走时直接清,不再依赖 EventAgentDone。

### 3.5 与 F-53 / F-61 的边界

| 文档 | 管什么 | F-62 不动 |
|------|--------|----------|
| F-53 `Message`/`Prompt`/`endPrompt` 语义 | submit / endPrompt / EventAgentDone 那一段 | ✓ 全部不动 |
| F-61 HungPrompt → respawn | agent hang 5min 后的 suspect/respawn 路径 | ✓ 独立 PR,见 §9 |
| F-61 bridge 死亡 → replayCh | bridge 死时把 in-flight 推到 queue | ✓ 旧版契约,但 F-62 让 §3.4 场景下不会再发生"死时塞 queue" |

---

## 4. 代码改动一览

| 文件 | 改动 |
|------|------|
| `internal/agentsession/session.go` | 新增 `ClearInFlight()` 方法 |
| `internal/chatsession/chatsession.go` `SetSelectedCwd` | §3.3.2 B: 切 cwd 之前清旧 AS.inFlight (release-then-persist) |
| `internal/chatsession/chatsession.go` `SetSelectedAgent` | §3.3.3 C: 切 agent 之前清旧 AS.inFlight (同上) |
| `internal/chatsession/chatsession.go` `LookupSelectedAgentSession` hadPrior 分支 | §3.3.3b D: 同 (agent, cwd) 复用时清旧 AS.inFlight,rename `newAS` → `existingAS` 表达语义 |
| `internal/chatsession/manager.go` `RestoreFromRegistry` | §3.3.1 A: 删 re-push 循环 + 注释 |

预计 diff: ~120 行新增,~25 行删除,纯局部。

---

## 5. 用户可见行为(契约)

| 场景 | 用户看到 | 修复后 |
|------|----------|--------|
| 同一 (agent, cwd) 内来回 | 正常 | 正常 |
| `/cwd` 切到另一个 cwd,旧 AS 有 in-flight | 旧消息一旦切走立即作废;新消息只送到新 AS | 同上 |
| daemon 重启,旧 AS 仍 detached | cs.queue 不再混入陈年 in-flight,只承接新消息 | 同上 |
| HungPrompt 5min 无响应 | 🔄 reaction 循环 + 日志告警 | 不变(F-61 已覆盖) |
| 同样的 OneShotModel 错贴卡 | 错贴到新消息的 receipt card | 消失(主仓 in-flight 在 /cwd 那一刻已清,不再混进 feat-dsh batch) |
| receipt card 已有 ✅ DONE 的消息 in-flight 未清 | ✅ DONE 与 in-flight 共存 | 消失(如果该消息对应 AS 在 /cwd 时已换,清空即生效) |

---

## 6. 可观测性

- **slog 新增**:`chatsession: SetSelectedCwd cleared old AS in-flight` (INFO, 带 `from_cwd` / `to_cwd` / `cleared_count`);`agentsession: ClearInFlight called` (DEBUG);`agentsession: persist after ClearInFlight failed` (WARN, 已有 pattern)
- **doctor**: 无新增字段;`PROBER` 块在 F-61 已覆盖 AS-level suspect
- **metrics**: 无,本期不动 metrics 包

---

## 7. 测试策略

### 7.1 单元测试

- `ClearInFlight` 幂等:空切片 no-op;非空切片 → nil + persist 一次
- `ClearInFlight` 并发:与 Submit / endPrompt 并发调用,看是否 race(用 `-race` 跑)
- `SetSelectedCwd` 旧 AS 清空: 写两个 AS 进 pool,改 cwd,断言旧 AS.inFlightMessages == nil 且 disk 写空
- `RestoreFromRegistry` 不 push: 注入有 inFlightMessages 的 AS,断言 `cs.queue.Length() == 0`
- `LookupSelectedAgentSession` hadPrior 清空: 旧 AS detached,断言旧 AS.inFlightMessages == nil

### 7.2 集成测试(`cmd/nightme/e2e_test/`)

- **场景 A(Bug 1 复现 → 消失)**:主仓上一条 hung 消息 → `/cwd feat-dsh` → 发新消息 → 验证 feat-dsh 的 batch 只有新消息,主仓消息不再被推
- **场景 B(Bug 2 复现 → 消失)**:用户发消息 → agent 部分响应但无 EventAgentDone → `/cwd` 切走 → 验证旧 AS.inFlight 磁盘写空,新 AS 不接收旧消息
- **场景 C(回归)**:同一 (agent, cwd) 内 5 条连发,合并 batch,3 个消息合并 1 个 Prompt,所有事件锚到最后一条 `LastMessageID`(F-53 既有契约不变)

### 7.3 回归

- F-53 message-prompt-lifecycle 全量
- F-61 bot-failure-recovery 全量(restored in-flight slice 字段保留供 audit,运行时仍可空)
- `cmd/nightme/test.go` / `cmd/nightme/test_test.go` 涉及 AS_lifecycle 的所有用例

---

## 8. 迁移 / Rollout

- **磁盘 schema**: 不变。`InFlightMessages` 字段保留,新算法只往磁盘写空数组,不再读回时 push queue
- **daemon 重启**: 三处改完即可,无需迁移
- **回滚**: 任何一处不放心,revert commit 即可,磁盘数据无新增 schema 字段
- **风险点**: `SetSelectedCwd` 加锁边界时要避免破坏现有 `cs.gitStatusDeps` 的 RefreshGitStatus 路径(它用的是 `cs.mu.RLock` 之外的路径),`ClearInFlight` 内部已经自取 `as.asMu`,不与 chat 锁嵌套
- **不发版公告**: 走 CHANGELOG.md `Unreleased` 段

---

## 9. 后续(Out of Scope)

- **F-61 续: Watchdog.onHungPrompt 调 endPrompt**(`PromptEndStalledKilled`)防止 in-flight 卡死
- **F-61 续: prober.respawnHung 真正接 Manager 引用**,把 respawn 回路打通
- **per-cwd 用户可见"已提交但 agent 未回"reaction 状态**(F-50 后续): 与 in-flight 路由解耦,本期不动
- **cs.queue 多 bucket 化**(每 (agent, cwd) 一个 queue): 范围比 F-62 大很多,本次不引入

---

## 10. 关联文档

- [`F-53-message-prompt-lifecycle`](./F-53-message-prompt-lifecycle.md) — `Message`/`Prompt`/`endPrompt` 语义源头
- [`F-61-bot-failure-recovery`](./F-61-bot-failure-recovery.md) — HungPrompt / respawn / replayCh 同一族问题
- [`F-runtime`](./F-runtime.md) — ChatSession / AgentSession 边界
- [`F-chat-session`](./F-chat-session.md) — InputBuffer FSM / TryFlush 入口
- [`F-message-flow`](./F-message-flow.md) — 全链路端到端切片
- [`../SPEC.md`](../SPEC.md) §4.3 — 并发约束与 EventCallback 切换
