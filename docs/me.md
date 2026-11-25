# me — 程序侧契约：settled 信号与 Outcome

> **Scope**: 每条 inbound 消息的 settled 信号、`Outcome` 词汇表与合并规则、各命令的 settle 时机
> **读者**: 编写驱动 nightme 的程序（`me`）的工程师
> **Related docs**:
> - [SPEC.md](./SPEC.md) 运行时与 gateway 架构
> - [SPEC.md §2.1](./SPEC.md) inbound 分发链
> - [CHANNEL.md](./CHANNEL.md) 多 channel 与消息渲染

---

## 0. 摘要

IM channel 上的反馈是给人看的自由文本卡片。程序驱动 nightme 时，卡片无法回答两个问题：

1. 这条消息**处理完了吗**？
2. 它**怎么结束的**？

settled 信号是与之并行的机器通道：每条 inbound 消息对应**恰好一条** `OutMessageSettled`，携带一个结构化的 `Outcome`。程序按 `Status` 分支，不解析文本。

settled 信号**不渲染**——所有 IM channel 静默丢弃它（见 §6）。它只在 channel 之外可见。

---

## 1. 消息形态

settled 是一条普通的 outbound message，`Kind` 为 `OutMessageSettled`（`String()` 为 `message_settled`）：

```go
type OutboundMessage struct {
	ChatID  string          // 消息所属的 chat
	Kind    OutboundKind    // OutMessageSettled
	ReplyTo string          // 这条 inbound 消息的 msgID
	Outcome *Outcome        // 结构化结论；其他 Kind 为 nil
}
```

`ReplyTo` 是 inbound 消息的 id，也就是程序自己发出去的那条消息的 id——程序据此把自己的请求和结论对上。

`Outcome`：

```go
type Outcome struct {
    Success    bool              // 这次工作成了没有
    Status     int               // HTTP 状态码
    StatusText string            // 一行人类可读的复述
    Data       map[string]string // 机器可读的事实（命令各自定义）
}
```

`Status` 是 HTTP 状态码。`2xx` 是正常分支（活干完了，或者没东西可干），`4xx` 与 `5xx` 是异常。`Status` 为 0 表示这次工作没有 HTTP 状态，这时 `Success` 由产出方自己说。

`StatusText` 是唯一的一行人话：命令做成了什么、或者什么坏了，都在这里。`Data` 是事实，不是分类码。

`Data` 的键由产生它的命令定义。当前在用的键：

| 键 | 含义 |
| --- | --- |
| `agent` | 跑这件事的 agent 名 |
| `issue` | `/gtw fix` 处理的 issue 号（本地模式为 `-1`）；`/gtw assign` 处理的是被贴 / 摘队列标记的 issue 号 |
| `issue_url` | issue 在平台上的链接（`/gtw assign` 给出被改动的那个 issue） |
| `branch` | 分支名 |
| `worktree` | worktree 路径 |
| `repo_root` | 主仓库根目录 |
| `mode` | `/gtw fix` 的模式（`remote` / `local`） |
| `selected_by` | issue 是怎么选出来的（`auto` = `/gtw fix *` 自己挑的） |
| `wake_after` | 队列里没有可选项时，个人 agent 多久之后再来（`"5m"`） |
| `label` | 挡住这次 fix 的 issue 状态 label（`nightme/wip` / `nightme/ready` / …） |
| `candidates` | `/gtw fix *` 看了多少个候选 issue（数字） |
| `worktree_candidates` | `/gtw back` 能跳过去的 worktree 分支（逗号分隔的分支名） |
| `held` | `/gtw fix *` 看了多少个被别的 agent 占用（带 `nightme/*` 状态 label）而不在候选里的 issue（数字） |
| `blocked` | 其中多少个因为前置依赖没落地被跳过 |
| `branch_exists` | 其中多少个因为本地已有同名分支被跳过 |
| `pr_open` | 其中多少个因为已经开了 PR 被跳过 |
| `topic` | `/gtw plan` 的主题 |
| `sha_range` | commit 区间（`<before>..HEAD`） |
| `commits` | commit 条数（`/gtw commit` 与 `/gtw push` 给出确切条数；`/gtw sync` 的卡片只列前 10 条，所以它不给这个键） |
| `commit_log` | commit 列表（每行一条 `<sha> <subject>`）——`/gtw commit` 报告"工作早就提交过了"时给出 |
| `changes` | `/gtw fix` 判完后，agent 留下的改动总数（改动的路径数 + commit 数） |
| `paths` | 其中改动了的路径数（同一个路径只算一次） |
| `staged` | 其中已经进了 index 的 |
| `unstaged` | 其中改了但没进 index 的 |
| `untracked` | 其中 git 没跟踪的 |
| `remote_head` | push 之后 `origin/<branch>` 指向的 sha（读不到时这个键不出现） |
| `pr_number` / `pr_url` | PR 的编号 / 链接 |
| `pr_state` | PR 自己的状态（`open` / `closed` / `merged`）——`/gtw pr view` 给出 |
| `base_branch` | PR 的目标分支——`/gtw pr view` 给出 |
| `conflict` | PR 现在有没有冲突（`"true"` / `"false"`）——`/gtw pr view` 给出 |
| `merge_state` | 平台中立的合并判定（`mergeable` / `conflicting` / `behind` / `unknown` / `blocked`）——`/gtw pr view` 给出 |
| `action` | `/gtw assign` 做的是哪一步（`added` / `removed`） |
| `groups` | review 跑了几组（`/review`） |
| `findings` | review 收集到的 findings 条数（`/review`） |
| `continued_groups` | 续跑完成了几组（`/review --continue` 只跑没完成的那几组：跑到 agent 的那几组续跑，没跑到的那几组重跑） |
| `unfinished` | 没跑完的项数（`/review` 的 fan-out 跑过就有：全部跑完时是 `0`；fan-out 之前就失败就没有这个键） |
| `delivered` | `/review` 的 findings 有没有交付给 agent（`"false"` = 这次 review 还没到齐，findings 还在等；`"true"` = 已经注入 agent） |
| `addressed` | `/review` 的 findings 有没有被处理（`"true"` = findings 交到 agent 手里之后，工作区多了新提交或新改动；`"false"` = 工作区没变）；没有 findings、agent 回合没有干净结束、工作区读不到时都没有这个键 |
| `subcommand` | 被 `/gtw abort` 取消的子命令 |
| `elapsed` | 被取消的子命令（`/gtw abort`）或 review（`/review --abort`）已经跑了多久 |
| `kind` | 被 `/review --abort` 取消的是哪一次 run：`review`（review 本体）或 `continue`（续跑） |
| `repo` | 仓库（`owner/repo`） |
| `issues` | `/gtw new -y` 开出来的 issue 编号（逗号分隔） |
| `kept` | 还没开成 issue 的草稿数 |
| `deps` | `/gtw new -y` 建成的依赖关系数（总是出现，没有依赖时是 0） |
| `deps_failed` | `/gtw new -y` 建不成的依赖关系数（总是出现，没有失败时是 0） |
| `lang` / `scope` | `/gtw config` 的语言与作用域（`global` / `repo` / `default`）；`/gtw commit -g --unset` / `/gtw pr -g --unset` 删掉全局默认 agent 时也是 `global` |
| `unset` | `/gtw config --unset` 删掉了这个设置（值是 `true`） |
| `cwd` | `/cwd` 设好的工作区路径 |
| `workflow` | `/me start` 启动的 workflow 的名字 |
| `run_id` | `/me start` 这次运行的 id |
| `iterations` | `/me start -i` 指定的轮数——没有这个 flag 时不出现 |
| `step` | `/me resume` 和 `/me next` 作用的那一步的 id：跳一段时是被点名的那一步（最后被跳过的那一步），重跑时是被重跑的那一步；命令没有动作时是它看的那一步 |
| `upstream_status` / `upstream_code` | 回合失败时上游报的状态码与它自己的码 |

值的写法是统一的：布尔是 `"true"` / `"false"`，数字是十进制字符串，枚举是小写 snake。**没有值时键不出现**，不用空串。

`Data` 刻意是 `map[string]string` 而不是 struct：不同命令产出不同的事实，程序只读自己关心的键。

---

## 2. Outcome 词汇表

`Status` 是 HTTP 状态码，程序按它的类别分支：

| Status | 含义 |
| --- | --- |
| `2xx` | 正常分支——活干完了，或者没东西可干 |
| `400` | 请求不合法：命令行、参数、工作区、issue 号、分支名 |
| `404` | 要的东西不在：issue、PR、worktree、续跑记录、workflow 草稿 |
| `409` | 与眼下的世界冲突：分支已存在、issue 被占、工作区脏、PR 没提交 |
| `429` | 上游在限流 |
| `499` | 有人把它停了（`/stop` 停掉的回合、`/gtw abort` 取消的命令、`/review --abort` 取消的 review） |
| `500` | 坏了：agent 没跑成、工作区读不了、状态文件坏了、回合死了 |
| `501` | agent 不支持这个动作 |
| `503` | 平台或上游不可用：label 写不上、provider 连不上、队列满了 |
| `0` | 没人报过结论 |

`2xx` 是正常分支，`4xx` 与 `5xx` 是异常——这一条由 `Status` 的类别决定，程序不必再看别的。

"没东西可干"也是 `2xx`：`/gtw fix *` 的队列空、`/gtw commit` 没有可提交的、`/gtw pr` 的 PR 已经在了——这些都报 `200`，**没有** `issue` 键或者 `commits=0` 这样的**事实**告诉程序是哪一种。

## 3. 合并规则

一条消息可能有多方参与（命令本身、它排入的 agent 回合），每方的结论合并成一条。

**失败的一方赢**：命令报"没东西可干"（`200`）盖不住回合死在上游 `429` 上。

**两边都失败时先报的一方赢**：命令的专属结论不被后到的通用结论顶掉。

**`Status`**：取胜出的一方。

**`StatusText`**：取胜出的一方；胜出的一方没有话时用另一方的。

**`Data`**：逐键合并，后上报的覆盖先上报的；只有一方拥有的键保留。

**没有状态的一方不算结论**：它只带事实，不说工作做得怎么样，所以它赢不过有状态的一方。`/gtw fix` 派出去的回合被停掉时就是这个情形——事实照样到程序手里，结论由回合给。

**没人报过结论的一方**（没有状态、没有话、没有事实）不算失败：说什么都没有不是坏了一件事。

**prompt-end 的兜底语义**：回合干净结束映射成 `200`，它是**通用兜底**信号——命令已经上报过结论时，它不覆盖那个结论（命令报 `200`，最终仍是 `200`）。回合非正常结束（失败 / 进程死 / 被杀）总是参与合并：失败是新信息。

## 4. settle 的时机

settled 不是"命令返回时"发，是"**这条消息引发的工作全部结束**"时发。

### 4.1 hold 模型

每条消息有一个共享的 `Work`，参与方各自持一份 hold：

- `cs.Hold(msgID)` 取一份 hold；
- `Work.Report(outcome)` 把结论并进这条消息；
- `Work.Release()` 放掉一份 hold；
- **最后一份 hold 放掉时**，通过 `cs.Emitter()` 发出**一条** `OutMessageSettled`。

同一个 msgID 的多方拿到的是同一个 `Work`，各自加一份 hold——所以"命令 + 它排入的回合"两方都结束时才 settle。

### 4.2 等回合结束

`cs.HoldUntilPromptEnd(msgID)` 持一份 hold，直到承载这条消息的 agent 回合结束。回合结束原因折进 `Status`：

| 回合结束原因 | Status |
| --- | --- |
| 干净结束 | `200` |
| 用户停止 / 用户杀死 | `499` |
| 卡死被杀 | `408` |
| 错误、进程死 | `500` |

回合失败时上游自己报的状态（`429` 之类）比这张表更准：它直接成为 `Status`，`Data` 里另带 `upstream_status` 与 `upstream_code`。

自动持 hold 的入口：

- `cs.QueueUserMessage` —— 排入队列的消息；
- `cs.SteerUserMessage` —— 被 steer 的文本；
- `/review` 与 `/review --continue` —— review 本身，以及它把 findings 注入进去的那个回合。

### 4.3 命令自身的 hold

命令分发（`inbound.runCommand`）在派发期间持一份 hold，**在命令回复发出之后**才放掉——settled 永远排在命令回复之后。

`/review` 的 hold 在 `Handle` 里**同步**取（不能在 goroutine 里取：那会晚于分发器的 release，导致消息在 review 开始前就 settle 成 `Status` 为 `0` 的结论），goroutine 退出时放掉。

`/gtw fix` 派发 issue 时走 `cs.QueueUserMessage`，它自己持一份 hold 到承载那条消息的回合结束——所以命令返回时消息**不** settle，它等 agent 把 issue 做完。派发失败时没有回合会结束，消息在命令返回时就 settle（结论是 `500`：worktree 建好了，issue 没送到 agent）。

---

## 5. 各命令的 settle 时机

| 命令 | agent 工作怎么跑 | settle 落在 |
| --- | --- | --- |
| 普通提问 | 会话自己的回合 | 回合结束 |
| `/queue` `/resume` | 排入队列 → 会话回合 | 回合结束 |
| `/gtw fix -y` `/gtw new` `/gtw plan` | 排入队列 → 会话回合 | 回合结束 |
| `/handoff` `/wiki init` | 排入队列 → 会话回合（命令自己等到回合结束） | 回合结束 |
| `/steer` | 文本进队首 → 下一个回合 | 承载该文本的回合结束 |
| `/gtw commit` `/gtw pr` | `agent.RunOnce` 同步跑在命令里 | 命令返回时（agent 已跑完） |
| `/gtw push` | 不跑回合（纯 git push） | 命令返回时 |
| `/gtw close` `/gtw back` `/gtw config` `/gtw sync` `/gtw abort` `/gtw assign` | 不跑回合 | 命令返回时 |
| `/cwd` `/use` `/think` `/tools` `/watch` | 不跑回合 | 命令返回时 |
| `/new` | 重置会话上下文（不跑回合） | 命令返回时 |
| `/stop` | 同步结束当前回合 | 被停止的**那条用户消息** settle 成 `499`；`/stop` 自己结算它停掉的那次停（`200` 或 `4xx` / `5xx`） |
| `/close` | 同步关闭会话（内部结束当前回合） | 被关闭的那条用户消息 settle 成 `499`；`/close` 自己 settle 成 `Status` 为 `0` 的结论 |
| `/review` `/review --continue` | review goroutine + 注入回合 | 两者都结束 |

`/stop` 与 `/close` 结束的是**别的** inbound 消息的回合——那条消息拿到 `499`，命令自己的消息独立结算。

---

## 6. 可见性

settled 信号**不渲染**。每个 IM channel 都静默丢弃 `OutMessageSettled`：它给程序看，不给人看。

人眼唯一能看到的地方是 daemon 日志里的一行：

```
chatsession: message settled chat_id=… message_id=… status=… summary=…
```

本机跑 daemon 时，这行的时间戳落在 agent 回合最后一个事件之后，就是"真的结束了"的证据。

---

## 7. 终局路径（不需要回合就 settle）

有些消息根本不会走到 agent 回合。它们各自直接结算：

| 情形 | Status |
| --- | --- |
| 命令返回错误 | `500` |
| 命令 panic | `500` |
| 队列拒绝这条消息 | `503` |
| 消息被丢弃（换 workspace 的边界清队列 / 队列重置） | `499` |
| 消息没有 @ 到 bot（watch 模式丢弃） | `200` |
| 没有 workspace | `400` |
| 没有 agent 起得来 | `500` |
| `/gtw fix *` 的队列里没有可选项 | `200`（没有 `issue` 键） |
| `/gtw commit` 的工作区没有改动 | `200`（`commits=0`） |
| `/gtw push` 的分支没有远端没有的 commit | `200`（`commits=0`） |
| `/review` 没有可 review 的 diff | `404` |
| `/review` 的 agent 不支持 review | `501` |
| `/review` 的 findings 没能注入 agent | `500` |

## 8. 旁听通道（Tracker）

settled 是一条消息的**结论**。程序往往还想知道这条消息**走到哪一步了**：daemon 收下了没有、agent 开始干活没有、是不是在等一个回答、最后怎么样了。

`internal/gateway/observe` 的 Tracker 就是这条通道：

| 能力 | 说明 |
| --- | --- |
| `Register(msgID, chatID, text)` | 登记一条程序自己发的消息 |
| `Get(msgID)` | 读这条消息的执行快照 |
| `Watch(msgID)` | 订阅这条消息的变化；settle 时通道关闭 |
| `Observe(outbound)` | 把一条出站消息折进它对应的执行 |

关联键是 `ReplyTo`：daemon 给每条它为某个回合产出的消息盖上那个回合的入站消息 id。Tracker 只认**登记过**的消息 id，所以人发的消息永远不会被算到程序头上。

没有 `ReplyTo` 的出站消息（只有 `OutCommandReply`）兜底落到该 chat 里 me 的 in-flight 锚点上——没有锚点就不归属。

执行的阶段只会往前走：`sent` → `queued` → `running` → `settled`。迟到的回复拉不回已经 settle 的执行。

---

## 9. 边界

- settled 只在**工作真正结束**时发出。一轮很长的 agent 回合会把 settled 往后推——这是设计意图：settled 报的是结束，不是开始。
- `/steer` 的文本由下一个 prompt-end 或下一条入队消息冲刷队列，settled 跟着承载它的那个回合。
- 没有任何一方上报结论时，settled 的 `Status` 是 `0`——它表示"没有结论"，不是"成功"。
- IM channel 丢弃 settled，所以从聊天窗口看不到它；要确认一条消息真的结束了，看 daemon 日志。
