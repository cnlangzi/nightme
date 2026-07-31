# nightme Docs — Overview & Classification Rules

> **目的**：本 README 解释 `docs/` 目录的**组织规则**，方便后续 contributor 一眼明白"我要写的东西应该放哪"。
> **本 README 不属于任何业务文档**——它是元文档（meta-document），描述 docs 本身的结构。

---

## 1. Overview

nightme 的文档按**内容性质**分 5 层，每层职责单一、不重叠：

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 5: PLAN.md          ← 实施计划（什么顺序、怎么拆 PR）│
├─────────────────────────────────────────────────────────────┤
│  Layer 4: feat/F-XX-*      ← 每个 feature 的实现（含代码）  │
├─────────────────────────────────────────────────────────────┤
│  Layer 3: FEATURES.md      ← 功能索引（每个 feature 一句话）│
├─────────────────────────────────────────────────────────────┤
│  Layer 2: SPEC.md          ← 技术架构（无代码）             │
├─────────────────────────────────────────────────────────────┤
│  Layer 1: PRD.md           ← 产品本身（无技术词汇）         │
└─────────────────────────────────────────────────────────────┘
```

**自上而下**：从抽象到具体，从产品到实现。
**阅读顺序**：新 contributor 从 PRD 开始读，按需往下钻。
**更新顺序**：业务/产品变更改 PRD → 架构变更改 SPEC → 新功能改 FEATURES + 新 feat/ → 实施计划改 PLAN。

---

## 2. 各层职责

| 层 | 文件 | 内容 | 不应包含 |
|----|------|------|----------|
| **PRD** | `PRD.md` | 产品定位、目标用户、使用场景、核心哲学、功能范围、范围外、成功标准 | 技术栈、架构、代码、文件路径 |
| **SPEC** | `SPEC.md` | 架构总览、组件、数据流、Session 生命周期、并发模型、技术栈、NFR、安全、技术决策 | Go 代码、JSON schema、YAML 配置、具体函数签名 |
| **FEATURES** | `FEATURES.md` | F-XX 功能列表（含状态、设计文档链接、release checklist） | 详细设计、代码、edge cases |
| **feat/** | `feat/F-XX-name.md` | 单个 feature 的详细设计：接口、struct、实现、edge cases、测试 | 跨 feature 的架构描述、产品定位 |
| **PLAN** | `PLAN.md` | M0-M3 里程碑、commit 拆分、验收标准、文件结构 | 产品决策、架构设计、代码 |

---

## 3. 分类决策树

> "我想加一段内容，应该放哪？"

```
Q1: 这是关于产品的（用户、场景、价值、范围），还是关于技术的（架构、实现、栈）？
│
├─ 产品 ──► PRD.md
│
└─ 技术
   │
   Q2: 这是整体架构（多个 feature 的关系），还是单个 feature 的细节？
   │
   ├─ 整体架构
   │  │
   │  Q3: 含不含代码（Go / JSON / YAML）？
   │  │
   │  ├─ 不含代码 ──► SPEC.md
   │  │
   │  └─ 含代码 ──► 拆：架构概念进 SPEC.md，实现细节进对应 feat/F-XX
   │
   └─ 单个 feature
      │
      Q4: 这是一个新的 feature，还是已有 feature 的迭代？
      │
      ├─ 新 feature ──► FEATURES.md 加一行 + 新建 feat/F-XX-name.md
      │
      └─ 已有 feature 迭代 ──► 修改对应 feat/F-XX-name.md（FEATURES.md 不动）
```

**特例**：
- **CLI 命令的使用方法 / 安装步骤**：放仓库根 `README.md`，不放 docs/
- **某个 feature 的命名 / 编号变更**：改 `FEATURES.md` 即可，不动 SPEC/PRD
- **新里程碑或 commit 计划变更**：改 `PLAN.md`
- **已合并到 feat/ 的代码示例被 SPEC 引用**：SPEC 里只描述概念 + 链接到 feat/，不重复代码

---

## 4. 命名约定

### 4.1 顶层文档

- 全大写 + `.md` 后缀：`PRD.md`、`SPEC.md`、`FEATURES.md`、`PLAN.md`
- 不带日期 / 版本号（版本号在文档内部的"更新日志"维护）

### 4.2 Feature 文档

- `feat/F-XX-kebab-case-name.md`
- `F-XX`：两位数字编号（01-99），保证字典序 = 逻辑序
- `kebab-case-name`：短横线连接的英文短语，描述 feature
- **不要**用中文文件名（保持 Git 工具链兼容）

### 4.3 Feature 编号规则

| 编号段 | 用途 |
|--------|------|
| F-01 ~ F-19 | 已分配（见 FEATURES.md） |
| F-20+ | 新增 feature 时续编，**不能复用旧编号** |

**调整编号**（删除/合并 feature）：v0.1 阶段允许重排；v0.2+ **禁止重排**（commit 链接、PR 描述里会引用编号）。

---

## 5. 文档交叉引用

### 5.1 引用语法

| 引用类型 | 语法 | 示例 |
|----------|------|------|
| 同目录文件 | `[NAME.md](./NAME.md)` | `[FEATURES.md](./FEATURES.md)` |
| 子目录文件 | `[NAME.md](./feat/NAME.md)` | `[F-01](./feat/F-01-session-create.md)` |
| 同目录文件 + 节 | `[NAME.md](./NAME.md) §X` | `[SPEC.md](./SPEC.md) §2.1` |
| 父目录 | `[NAME.md](../NAME.md)` | （docs/ 子目录引用仓库根时）|

### 5.2 引用方向（避免循环）

```
PRD.md  ──► SPEC.md
            │
            └────► FEATURES.md
                          │
                          └────► feat/F-XX
                                        │
                                        └────► PLAN.md

（feat/ 可以反向引用 SPEC 和 FEATURES，但不能引用 PRD 的产品哲学细节）
```

**禁止**：
- PRD.md 引用 feat/（PRD 不应该知道实现细节）
- SPEC.md 引用 feat/ 的代码（SPEC 应该描述概念，链接到 feat/ 看细节）
- PLAN.md 引用 PRD（PLAN 假设产品定位已经冻结）

---

## 6. 文档维护工作流

### 6.1 改动触发

| 触发 | 改哪些文档 |
|------|------------|
| 产品定位变化 / 新增用户群 / 范围调整 | PRD.md |
| 架构变更 / 换技术栈 / 新增组件 | SPEC.md |
| 新增 feature | FEATURES.md + 新 feat/F-XX |
| 已有 feature 设计调整 | feat/F-XX |
| 里程碑 / commit 计划变更 | PLAN.md |
| 跨多个 feature 的设计变更 | SPEC.md + 对应 feat/ 都改 |

### 6.2 每次改动的 checklist

- [ ] 改动符合分类规则（见 §3 决策树）
- [ ] 更新相关文档的**交叉引用**（章节号改了要同步更新）
- [ ] 更新文档内部的**更新日志**（v1.0 → v1.0r → v1.0s ...）
- [ ] 跑 `grep` 检查"哪些文档引用了被改的章节号"

### 6.3 改动提交格式

```
docs: <简短描述>

- 触发原因
- 改了哪些文件
- 影响哪些引用
```

---

## 7. Anti-patterns（不要这样做）

❌ **PRD.md 里出现"PTY"、"WebSocket"、"Go goroutine"等术语**
→ 这些是实现细节，应在 SPEC 或 feat/

❌ **SPEC.md 里贴 Go 代码块、JSON schema**
→ SPEC 只描述概念 + 链接到 feat/

❌ **FEATURES.md 里写详细的设计说明**
→ FEATURES 只索引，详细内容在 feat/F-XX

❌ **feat/F-XX.md 里讨论产品哲学**
→ 哲学在 PRD.md，feat/ 只谈实现

❌ **PLAN.md 里设计架构**
→ PLAN 假设 SPEC 已经定稿，只谈实施

❌ **每个 feature 一个独立的架构章节**
→ 架构在 SPEC.md 集中描述，feat/ 引用即可

---

## 8. 文档结构示意

```
docs/
├── README.md              ← 本文件（元文档）
├── PRD.md                 ← Layer 1: 产品
├── SPEC.md                ← Layer 2: 技术架构
├── FEATURES.md            ← Layer 3: 功能索引
├── PLAN.md                ← Layer 5: 实施计划
└── feat/                  ← Layer 4: 每个 feature 的实现
    ├── F-01-session-create.md
    ├── F-02-message-passthrough.md
    ├── ...
    └── F-19-cli-bridge.md
```

---

## 9. 当你不确定时

按这个顺序自检：

1. **这是什么内容？**（产品 / 架构 / feature 实现 / 实施计划）
2. **现有 5 个顶层文件 + feat/，哪一个最匹配？**
3. **它是否含有代码 / schema？**  → 如果是，去 feat/
4. **它是新内容还是迭代？**  → 决定改 FEATURES 还是改 feat/
5. **检查交叉引用是否需要更新**

如果仍然不确定，**先放 SPEC.md 或 feat/，review 时再调整**——比放错位置然后搬动成本低。

---

## 10. 变更历史

- **v1.0**：建立 5 层文档结构 + 命名约定 + 决策树
