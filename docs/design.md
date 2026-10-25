# troupe 设计文档

> 状态：草案。本文记录已对齐的结论，未定事项见文末「待定」。

实现语言：Go。

## 1. 定位

troupe 面向手里有多个 coding agent 的用户。

- 各家 agent 的 harness 不同、擅长的事也不同。troupe 把它们按**角色**组合起来，让每个 agent 发挥长处、避开短处。
- **任务持久化和编排由框架负责**，不再交给 agent。agent 保持简单、无状态：做完一个有界任务，写下结果，然后退出。
- 用户用一句话描述需求，系统一直推进到完成；限流、等待、重试等细节对用户隐藏。
- 与 magpie 这类模型网关互补：magpie 管**一个 agent 背后接哪些模型**，troupe 管**多个 agent 怎么分工干活**。

## 2. 核心模型

### 2.1 角色

leader、coder、reviewer 等都是可配置的角色：

| 角色 | 职责 |
| --- | --- |
| leader | 拆解需求，编写验收标准 |
| coder | 按任务修改代码 |
| reviewer | 审查改动 |
| …… | 可按需扩展 |

每个角色绑定哪个 agent，由用户在配置中指定。

### 2.2 协作方式

协作方式也由用户选择，例如：

- **单人干**：一个 agent 独立完成。
- **A 写 B 审**：一个 agent 写，另一个 agent 审。
- **多版本择优**：多个版本中择优（best-of-N）。

## 3. MVP 范围

- 一次只跑一个任务，不做并行。
- 不做失败后自动切换 agent，留到后续版本。

## 4. 任务协议

框架与 agent 之间通过文件交互：框架写 `task.yaml`，agent 回 `result.json`。

### 4.1 task.yaml（框架 → agent）

| 字段 | 说明 |
| --- | --- |
| `id` | 任务 id |
| `goal` | 任务目标 |
| `role` | 本轮扮演的角色 |
| `workdir` | 工作目录 |
| `acceptance` | 验收命令列表 |
| `context` | 补充上下文 |
| `budget.max_minutes` | 时间预算（分钟） |

```yaml
id: t-20261017-001
goal: 为配置加载增加环境变量覆盖，并补充单元测试
role: coder
workdir: /path/to/repo
acceptance:
  - go build ./...
  - go vet ./...
  - go test ./...
context: |
  配置加载代码在 internal/config 下。
budget:
  max_minutes: 30
```

### 4.2 result.json（agent → 框架）

| 字段 | 说明 |
| --- | --- |
| `id` | 对应的任务 id |
| `status` | `done` / `failed` / `blocked` |
| `summary` | 做了什么 |
| `commit` | 产出的提交 |
| `notes` | 备注；blocked 时写明原因 |

```json
{
  "id": "t-20261017-001",
  "status": "done",
  "summary": "新增环境变量覆盖逻辑及对应测试",
  "commit": "a1b2c3d",
  "notes": ""
}
```

### 4.3 原则

1. agent 只做四件事：读 task、改代码、写 result、退出。
2. **以 acceptance 命令跑通为准**，`status` 只作参考。
3. 每一轮的完整记录按任务 id 存档。
4. MVP 的验收只跑命令；以后可以增加 review 类型的验收。

## 5. blocked 处理

当 agent 返回 `blocked`：

1. 任务暂停，原因写进 `notes`。
2. 通过可配置的通知方式提醒用户。MVP 只做**命令行输出**和 **webhook**。
3. 用户回复后，执行 `troupe resume <id>` 继续。

## 6. agent 接入

每家 agent 一个很薄的适配器，统一到同一个 Go 接口之后：

- 以无交互模式调用其命令行，例如 `codex exec`、`claude -p`；
- 把 task 作为提示词传入；
- 设置超时；
- 日志存档；
- 运行结束后读取 `result.json`。

接口草图：

```go
// Agent 是各家 coding agent 适配器的统一接口。
type Agent interface {
    // Name 返回适配器名称，如 "codex"、"claude"。
    Name() string
    // Run 以无交互模式执行一轮任务：传入 task，超时由 ctx 控制，
    // 日志写入 runDir，结束后读取并返回 result.json。
    Run(ctx context.Context, task Task, runDir string) (Result, error)
}
```

## 7. 存储

定义 `Store` 接口，底层实现可替换；MVP 使用 JSON 文件实现。

```go
type Store interface {
    SaveTask(ctx context.Context, t Task) error
    GetTask(ctx context.Context, id string) (Task, error)
    ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error)
    // UpdateStatus 仅当当前状态等于 from 时才改为 to，否则拒绝，
    // 防止并发写入互相覆盖。
    UpdateStatus(ctx context.Context, id string, from, to Status) error
    SaveRun(ctx context.Context, r Run) error
    ListRuns(ctx context.Context, taskID string) ([]Run, error)
}
```

目录布局：

```
.troupe/
├── tasks/
│   └── <id>.json          # 任务及其状态
└── runs/
    └── <id>/
        └── <n>/           # 第 n 轮
            ├── task.yaml
            ├── result.json
            └── logs/
```

写文件时先写临时文件，再通过改名（rename）替换目标文件。

## 8. 第一个场景

用 Go 重写一个开源 coding agent，先接入 Codex 和 Claude Code。

## 9. 待定

- 配置文件格式与角色绑定的具体写法。
- 任务状态机的完整状态集合。
- review 类型验收的设计。
- 并行执行与失败自动切换 agent（MVP 之后）。
