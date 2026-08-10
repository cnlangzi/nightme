# nightme Agent 落地计划

> **历史参考**:实施已完成,见 `wip/agent.md` 末尾的提交链。
> 下文是原始计划,跟实际落地有偏差的地方见 `wip/agent.md` 的"实施记录"。

---

## 总览

把当前 `internal/agent/agent.go` 里的 `AgentSpec` interface + `Agent` interface,替换成 `Info` struct + `Agent` struct + `Starter` interface 三件套。把每个 bridge 内部的 `Agent` struct 拆成 `driver`(协议细节)+ `starter`(注册对象)两个类型。

---

## 阶段划分

| 阶段 | 范围 | 退出条件 |
|---|---|---|
| **P0** | 设计定型 | 答掉 design doc 末尾的两个问题;双轨方案拍板 |
| **P1** | agent 包骨架 + claudecode pilot | claudecode + chatsession 测试全绿;旧 API 仍可用 |
| **P2** | pi / codex / acp 三桥套模板 | 4 桥都迁完,旧 API 仍可用 |
| **P3** | 上游调用方切换 | registry / spawner / agentsession / agents_cmd 全部用新 API |
| **P4** | 拆除旧 API | 删 AgentSpec / 旧 Agent interface;全仓 grep 无引用 |

---

## P0 — 设计定型

1. **driver interface 方法集**——SendText/SendBlocks/SendPermission/Reset/Close
2. **Agent.sessionID 并发原语**——`atomic.Value`
3. **双轨 vs 一次性切换**——双轨过渡:`LegacyRegister` + `AsStarter` adapter,旧 interface 保留到 P4

---

## P1 — agent 包骨架 + claudecode pilot

新增 `Info` struct、`LiveAgent` struct(临时名,P4 改回 `Agent`)、`Starter` interface、`driver` interface。

claudecode 拆分:`Agent` → `driver` (runtime + 5 methods) + `starter` (template + Info/Detect/Start)。

---

## P2 — pi / codex / acp / pty

按 claudecode 模板套。最终 5 个桥(claudecode/pi/codex/acp/pty)都迁完。

---

## P3 — 上游调用方切换

`Registry.Specs() []AgentSpec` 删除,`Builtins.List() []Starter` 返回。`Spawner.Spawn` 返回 `*LiveAgent`,`AgentSession.handle` 改为 `*LiveAgent`。`nightme agents` 用 `Info` 渲染。

---

## P4 — 拆除旧 API

- 删除 `AgentSpec` interface
- 删除 `Agent` interface
- 删除 `LegacyRegister` + `AsStarter` + `WrapAsAgent`
- `LiveAgent` → `Agent` 重命名
- `NewLiveAgent` → `NewAgent` 重命名
- 全仓 `grep -rn "AgentSpec"` 必须空

---

## 实际工作量和现实

| 维度 | 计划 | 实际 |
|---|---|---|
| 总 commit | 5 | **10**(分步提交) |
| agent 包改动 | ~200 行 | 467 行(新增为主) |
| 桥改动 | 4×250 | 4×~200(净减少,driver+starter 分得干净) |
| 测试改动 | ~100 | ~300(test fakes 重设计) |
| 时间 | 3 天 | 远超(实际工作量 + 文档同步) |

---

## 经验教训

1. **driver interface 包私有时,外部 bridge 用本地别名做编译期检查**(`agentDriver` 模式)。
2. **Spawner.Spawn 返回类型切换必须配合 WrapAsAgent adapter**,否则测试 mock 全部 type-assert 失败。
3. **`AgentSession.handle` 保留 interface{}**(而非 `*Agent`)让测试可以 type-assert 到 spy 类型——双轨期唯一的务实选择。
4. **测试 mock 持有 spy 引用直接访问**,而不是 `Handle().(*spy).field`,是更清晰的设计。但要求所有测试同步重构。
5. **真实 bridge 注册与 pty fallback 注册要统一接口**——pty 之前单独走 LegacyRegister,跟其他 4 桥路径不一致,统一后干净。

---

## 不在本计划范围

- `nightme agents` 命令的 UI/输出格式变更(只切换数据来源,不改展示)
- bridge 内部的协议 bug 修复
- 测试覆盖率提升