# 知微 Zhiwei
## AI 全时生活记忆与个人智能体

**产品名称：知微 Zhiwei**  
**产品类型：AI 全时生活记录 + Personal Memory + Personal Agent**  
**核心形态：知微智能硬件 + 知微 App + 知微 Cloud + 知微 Agent**

**品牌理念：**

> **见微知著，记住生活，看见自己。**

---

# 一、产品概述

## 1.1 产品愿景

知微是一款通过 AI 硬件持续感知用户生活，并通过 AI 自动理解、整理、记忆和跟进的个人智能体。

用户不需要主动记录。

只需要正常生活：

> 工作、学习、聊天、开会、运动、思考、旅行、吃饭、社交……

知微通过智能硬件持续采集声音，通过手机和云端 AI 完成：

- ASR
- 声纹识别
- 事件识别
- 人物识别
- 记忆提取
- Todo 提取
- Topic 识别
- Project 跟踪
- 风险识别
- 进度分析
- AI Review
- Personal Insight

最终帮助用户：

> **记住发生过什么，理解正在发生什么，并知道接下来应该做什么。**

---

# 二、品牌定位

## 2.1 为什么叫「知微」

“知微”取自：

> **见微知著**

用户每天的生活中会产生大量微小的信息：

- 一句话
- 一个想法
- 一次会议
- 一次承诺
- 一个决定
- 一次情绪变化
- 一个突然出现的灵感
- 一次与朋友的交流

单独看，这些信息都很微小。

但当这些信息经过长期积累之后，就会形成：

> **一个人的生活轨迹、目标、习惯、关系和变化。**

知微希望做的事情就是：

> **从微小的信息中发现重要的事情。**

---

# 三、产品定位

知微不是：

- AI 录音笔
- AI 转写工具
- AI 笔记工具
- Todo App
- 日记 App

而是：

# Personal Life Intelligence

即：

> **个人生活智能体。**

产品闭环：

```text
感知
 ↓
理解
 ↓
记忆
 ↓
组织
 ↓
跟进
 ↓
行动
 ↓
回顾
 ↓
再次理解
```

---

# 四、核心产品理念

## 4.1 从“记录”升级到“理解”

传统录音产品：

```text
录音
 ↓
转文字
 ↓
结束
```

知微：

```text
持续录音
 ↓
ASR
 ↓
人物识别
 ↓
事件识别
 ↓
事实提取
 ↓
Memory
 ↓
Topic
 ↓
Project
 ↓
Todo
 ↓
Risk
 ↓
Insight
 ↓
Action
 ↓
Review
```

---

# 五、核心价值

知微解决五个问题：

## 5.1 帮我记住

> “我上周和 Alice 讨论了什么？”

## 5.2 帮我整理

> “最近我主要在忙什么？”

## 5.3 帮我跟进

> “这个项目现在进展到哪里了？”

## 5.4 帮我发现问题

> “为什么这个事情一直没有推进？”

## 5.5 帮我认识自己

> “最近我的状态有什么变化？”

最终形成：

> **记录生活 → 理解生活 → 改变生活**

---

# 六、产品核心对象

知微不应该以“录音文件”为核心对象。

核心数据模型应该是：

```text
User
 │
 ├── Person
 │
 ├── Memory
 │    ├── Transcript
 │    ├── Event
 │    ├── Decision
 │    ├── Todo
 │    ├── Idea
 │    └── Insight
 │
 ├── Topic
 │    ├── Goal
 │    ├── Todo
 │    ├── Risk
 │    ├── Progress
 │    └── Review
 │
 ├── Project
 │    ├── Milestone
 │    ├── Task
 │    ├── Decision
 │    └── Risk
 │
 ├── Plan
 │
 └── Review
```

其中：

> **Audio 是原始数据。**

> **Memory 是事实层。**

> **Topic / Project 是组织层。**

> **Agent 是智能操作层。**

---

# 七、产品信息架构

知微 App 建议采用五个一级模块：

```text
首页
Echo
Topics
Review
我的
```

---

## 7.1 首页 Home

展示：

- 今日摘要
- 今日重要事件
- 当前 Topic
- Todo
- 风险
- AI Insight
- 快速询问知微

---

## 7.2 Echo

Echo 不代表竞品名称，而作为知微内部的：

> **生活时间线 / Memory Timeline**

展示：

```text
08:32
早餐

09:15
工作会议

10:43
与 Alice 讨论项目

14:30
产品灵感

18:40
健身

21:20
Rust 学习
```

---

## 7.3 Topics

管理：

- 学习
- 工作
- 健身
- 生活
- 项目
- 旅行
- 兴趣
- 自定义 Topic

---

## 7.4 Review

提供：

- Daily Review
- Weekly Review
- Monthly Review
- Topic Review
- Project Review
- Personal Insight

---

## 7.5 我的

包括：

- 我的设备
- 我的声音
- 我的记忆
- 我的联系人
- 数据与隐私
- AI 偏好
- 通知设置
- 数据导出

---

# 八、核心功能一：全时录音

## 8.1 智能硬件

知微硬件负责：

- 持续录音
- VAD
- 降噪
- 音频编码
- 本地缓存
- BLE
- 可选 Wi-Fi
- 电量管理
- 时间同步
- 固件 OTA

---

# 九、硬件不能完全依赖手机

硬件必须具备独立缓存。

原因：

- 手机 App 被杀
- 蓝牙断开
- 手机没电
- 网络断开
- App Crash
- 手机后台限制

都不能导致用户数据丢失。

推荐：

```text
Mic
 ↓
DSP
 ↓
VAD
 ↓
Opus
 ↓
Encrypted Flash
 ↓
BLE
 ↓
Mobile
```

建议硬件至少：

**8～32GB Flash**

---

# 十、音频编码

不建议长期保存 PCM。

推荐：

> **Opus**

例如 32kbps：

```text
≈ 14MB / hour
≈ 336MB / day
≈ 10GB / month
```

可以为硬件提供非常充足的离线缓存能力。

---

# 十一、硬件通信

推荐：

```text
BLE
+
Wi-Fi
```

正常状态：

```text
Hardware
 ↓ BLE
Mobile
 ↓ HTTPS
Cloud
```

大量同步：

```text
Hardware
 ↓ Wi-Fi
Cloud
```

---

# 十二、实时处理

目标不是每一句话都立即完成全部 AI 理解。

采用：

> **Micro-batch Streaming**

例如：

```text
Audio
 ↓
5～15 秒 Chunk
 ↓
Streaming ASR
 ↓
Speaker
 ↓
Semantic Processing
```

目标：

| Pipeline | 目标 |
|---|---:|
| Hardware → Mobile | <1s |
| Mobile → Server | <2s |
| ASR | 2～5s |
| Speaker | 3～10s |
| Semantic | 5～20s |
| Todo | <30s |

用户体验应该做到：

> **“刚发生的事情，很快就被知微记住了。”**

---

# 十三、录音分段

不能简单：

```text
每 60 秒一个文件
```

应该结合：

- VAD
- Silence
- Speaker Change
- Conversation Boundary
- Semantic Boundary

形成：

```text
Conversation Session
 ├── Segment
 ├── Segment
 └── Segment
```

---

# 十四、ASR Pipeline

```text
Audio
 ↓
Audio Quality
 ↓
VAD
 ↓
Noise Reduction
 ↓
Streaming ASR
 ↓
Punctuation
 ↓
Speaker Diarization
 ↓
Entity Recognition
 ↓
Semantic Understanding
```

输出：

```json
{
  "segment_id": "...",
  "start_time": "...",
  "end_time": "...",
  "speaker": "speaker_02",
  "text": "...",
  "confidence": 0.94
}
```

---

# 十五、声纹识别

用户首次使用知微：

> “录制你的声音，让知微认识你。”

建立：

```text
User Voiceprint
```

同时支持标注其他人物：

```text
Alice
Bob
老板
妈妈
Tom
```

---

# 十六、人物系统

人物页面：

```text
Alice

最近互动
├── 8/18 项目讨论
├── 8/15 工作沟通
└── 8/12 午餐

共同 Topic
├── 项目 A
└── 招聘

相关 Todo
├── 给 Alice 发方案
└── 确认会议时间
```

---

# 十七、Unknown Speaker

识别到未知人物：

```text
Unknown Speaker
```

用户可以：

> “这是 Alice。”

系统重新关联：

```text
历史 Transcript
Memory
Topic
Todo
Timeline
```

---

# 十八、Memory Engine

知微必须区分：

```text
Audio
 ↓
Transcript
 ↓
Event
 ↓
Memory
 ↓
Knowledge
```

例如：

### Transcript

> “最近工作有点忙，我可能要把 Rust 学习暂停一下。”

### Event

> 用户考虑暂停 Rust 学习。

### Memory

> Rust 学习计划可能受到工作压力影响。

### Knowledge

> Rust 学习存在延期风险。

---

# 十九、Memory 类型

```text
Event
Fact
Decision
Todo
Idea
Preference
Relationship
Goal
Milestone
Problem
Insight
```

---

# 二十、事实等级

所有 AI 信息需要标记：

```text
Observed
Inferred
Predicted
Suggested
```

例如：

### Observed

> 用户说：“周五上线。”

### Inferred

> 项目 Deadline = 周五。

### Predicted

> 当前进度可能无法按时上线。

### Suggested

> 建议今天确认测试状态。

---

# 二十一、Todo 自动提取

### Explicit Todo

> “明天记得给 Tom 发邮件。”

生成：

```text
Todo
给 Tom 发邮件

Due
Tomorrow
```

### Implicit Todo

> “这个接口还没测，发布前得把它搞定。”

生成：

```text
Todo
完成接口测试
```

低置信度 Todo：

> “要不要把这个加入 Todo？”

避免 AI 生成大量垃圾任务。

---

# 二十二、Decision

识别：

> “我们决定下周上线。”

生成：

```text
Decision

Project
XXX

Decision
下周上线

Participants
Alice / Bob / User

Status
Active
```

---

# 二十三、Topic

Topic 是知微区别于录音工具的核心能力。

用户可以创建：

```text
Rust 学习
减肥
健身
OpenClaw
桌面宠物
旅行
找工作
家庭装修
```

也可以让知微自动发现。

---

# 二十四、Topic 自动发现

如果过去 7 天反复出现：

```text
Rust
ownership
borrow
cargo
async
```

知微主动询问：

> **“我发现你最近一直在学习 Rust，要不要创建一个「Rust 学习」Topic，我帮你持续跟进？”**

---

# 二十五、Topic 数据结构

```text
Topic
├── Goal
├── Timeline
├── Progress
├── Todo
├── Risk
├── Decision
├── People
├── Related Memory
├── Next Action
└── AI Review
```

---

# 二十六、Topic 生命周期

```text
Created
 ↓
Active
 ↓
Progressing
 ↓
Blocked
 ↓
Completed
 ↓
Archived
```

---

# 二十七、Topic Progress

不采用简单的“AI 猜百分比”。

综合：

```text
Goal Progress
+
Task Completion
+
Milestone Completion
+
Activity Frequency
+
Recent Momentum
```

例如：

```text
Rust 学习

████████░░ 72%

完成
8 / 11 Tasks

当前阶段
Async

最近状态
连续 3 天学习

风险
工作时间增加

Next Action
完成 Tokio 基础练习
```

---

# 二十八、Project

Project 比 Topic 更强调：

> **目标 + 截止时间 + 交付结果**

例如：

```text
Project
两周完成 XXX 项目

Start
8/18

Deadline
9/1

Milestones
├── 需求
├── 开发
├── 测试
└── 发布
```

---

# 二十九、Risk Engine

自动发现：

### 时间风险

截止时间临近。

### 进度风险

连续多天没有进展。

### 依赖风险

任务被其他任务阻塞。

### 人员风险

依赖的人长期没有反馈。

### 目标风险

计划和实际行为长期不一致。

---

# 三十、Alternative Plan

发现风险后，不只提醒。

提供：

```text
方案 A
增加投入

方案 B
缩小目标

方案 C
延长周期

方案 D
暂停当前项目
```

例如：

> “你原计划 14 天完成 Rust 学习，目前完成度约 35%。按照当前速度可能需要 26 天。”

---

# 三十一、Agent

知微 Agent 是整个产品的智能入口。

用户可以直接：

> “我最近在忙什么？”

> “我最近为什么一直没学 Rust？”

> “把昨天关于项目的总结改一下。”

> “昨天我答应了谁什么事情？”

> “找出所有还没有完成的 Todo。”

---

# 三十二、Agent Tool Framework

核心 Tool：

```text
search_memory
get_memory
update_memory
delete_memory

search_person
update_person

create_todo
update_todo
complete_todo

create_topic
update_topic

get_topic_progress
get_topic_risks

create_reminder

generate_summary
generate_review

correct_fact
merge_memory
split_memory
```

Agent：

```text
LLM
 ↓
Planner
 ↓
Tool
 ↓
Permission
 ↓
Service
 ↓
DB
```

---

# 三十三、AI 纠错

用户：

> “不是周五，是下周三。”

知微：

```text
原信息
Friday

修改为
Next Wednesday

影响
├── Project
├── Todo
├── Reminder
└── Review
```

---

# 三十四、Daily Review

每天生成：

# 「今天，知微记得」

内容：

```text
今日概览

今天你主要做了 4 件事情。

工作
Rust 学习
健身
项目沟通
```

---

## 今日重要事件

---

## 今日完成

---

## 今日 Todo

---

## 今日 Decision

---

## 今日风险

---

## 今日灵感

---

## 今日人物

---

## 今日 AI Insight

例如：

> 今天你 3 次提到“时间不够”。

> 最近一周类似表达出现了 8 次。

---

## 明天建议

```text
1. 完成接口测试
2. 给 Alice 回复方案
3. 继续 Rust ownership
```

---

# 三十五、Weekly Review

每周生成：

```text
过去 7 天

工作       46%
学习       18%
生活       16%
社交       10%
其他       10%
```

展示：

```text
完成 Todo
17

新增 Topic
2

完成 Project
1

风险
2

重要变化
3
```

---

# 三十六、Personal Insight

知微最重要的高级能力之一。

例如：

> “过去 30 天，你有 7 次提到换工作，其中 5 次发生在晚上。”

> “你最近经常计划晚上学习，但实际执行主要发生在周末。”

> “你最近同时推进 8 个项目，其中 3 个超过 7 天没有进展。”

---

# 三十七、AI 漫画

Daily Review 支持：

```text
Summary
 ↓
Storyline
 ↓
Scene Planning
 ↓
Image Generation
 ↓
Comic Layout
```

例如：

```text
┌──────────────┐
│ 今天的你      │
│              │
│ “我要学 Rust” │
└──────────────┘

┌──────────────┐
│ 下午          │
│              │
│ 会议 × 3      │
└──────────────┘

┌──────────────┐
│ 晚上          │
│              │
│ “明天继续...” │
└──────────────┘
```

这是知微非常重要的情绪价值和分享场景。

---

# 三十八、Timeline

知微 Timeline：

```text
08:32
早餐

09:15
工作会议

10:43
与 Alice 讨论项目

14:30
产品灵感

18:40
健身

21:20
Rust 学习
```

支持：

```text
时间
类别
人物
Topic
Project
关键词
```

---

# 三十九、分类

默认：

```text
工作
学习
生活
健康
社交
家庭
灵感
旅行
财务
其他
```

AI 自动分类。

用户可以自定义。

---

# 四十、首页设计

```text
早上好 👋

今天是 8 月 18 日

────────────────

你的生活

3 Active Topics
2 Important Todos
1 Risk

────────────────

🔥 最近关注

OpenClaw 项目

███████░░ 70%

────────────────

📌 今天

09:30
项目会议

11:20
决定周五上线

────────────────

⚠️ 知微提醒

Rust 学习已经 3 天没有推进

────────────────

🧠 知微发现

你最近似乎同时推进了太多项目。

────────────────

[ 问问知微 ]
```

---

# 四十一、Reminder

分三级。

## Passive

> “你还有 3 个 Todo。”

## Smart

> “你昨天提到要给 Alice 发邮件，现在还没有相关记录。”

## Proactive

> “你们周二决定本周上线 XXX，但目前没有检测到测试完成。建议今天确认一下。”

默认：

> **少打扰。**

---

# 四十二、用户惊喜度

知微的核心 WOW 不是：

> “AI 很聪明。”

而是：

> **“它居然记得。”**

---

## 场景一

用户：

> “我下个月想去日本。”

三周后：

> “你之前提到想去日本，目前还没有开始规划，要不要我帮你整理？”

---

## 场景二

用户：

> “Alice 最近挺重要。”

之后：

> “你最近和 Alice 有 5 次沟通。”

---

## 场景三

用户：

> “我要减肥。”

知微持续发现：

```text
饮食
运动
睡眠
体重
```

形成：

```text
Goal
 ↓
Behavior
 ↓
Progress
 ↓
Risk
 ↓
Recommendation
```

---

# 四十三、Privacy First

全天候录音是知微最大的隐私挑战。

必须：

- 数据加密
- 用户明确知道录音状态
- 硬件录音指示
- 用户自主删除
- 用户数据导出
- Private Mode
- 数据生命周期控制
- 第三方人物隐私处理
- 数据权限隔离

---

# 四十四、Private Mode

用户可以：

> 一键停止录音。

硬件：

```text
Recording
 ↓
Private Mode
 ↓
Hardware Stop
```

---

# 四十五、数据生命周期

```text
Raw Audio
 ↓
Transcript
 ↓
Structured Event
 ↓
Memory
 ↓
Embedding
```

支持分别设置：

```text
Audio
30 / 90 / 365 days

Transcript
长期

Memory
长期
```

---

# 四十六、客户端技术

推荐：

> **Flutter + Native Audio/BLE**

Flutter：

```text
UI
State
Repository
Sync
Agent
Timeline
Memory
Topic
```

Native：

```text
BLE
Audio
Background
Power
Notification
OTA
```

---

# 四十七、本地数据库

推荐：

> SQLite + Drift

本地保存：

```text
Device
Audio Metadata
Transcript Cache
Memory Cache
Topic Cache
Todo Cache
Sync State
```

---

# 四十八、同步

采用：

```text
HTTPS
+
WebSocket
```

Audio：

```text
Hardware
 ↓
Mobile Queue
 ↓
Upload
 ↓
Server ACK
 ↓
Delete Local Cache
```

必须支持：

```text
Idempotency
Sequence
Retry
Resume
Checksum
```

---

# 四十九、服务端架构

```text
                     知微 Hardware
                           │
                           ▼
                     知微 Mobile
                           │
                     HTTPS / WS
                           │
                           ▼
                     API Gateway
                        tRPC-Go
                           │
          ┌────────────────┼────────────────┐
          │                │                │
       Audio           Core             Agent
       Service        Service           Service
          │                │                │
          ▼                ▼                ▼
         OSS             MySQL         AI Gateway
          │                │                │
          └───────┐        │        ┌──────┼──────┐
                  ▼        ▼        ▼      ▼      ▼
                  MQ     Redis      ASR    LLM   Image
                  │
          ┌───────┼────────┐
          ▼       ▼        ▼
         ASR   Speaker  Understanding
          │       │        │
          └───────┼────────┘
                  ▼
             Memory Engine
                  │
        ┌─────────┼──────────┐
        ▼         ▼          ▼
      Topic      Todo       Risk
        │         │          │
        └─────────┼──────────┘
                  ▼
                Agent
                  │
                  ▼
              Review Engine
```

---

# 五十、服务端技术栈

| 模块 | 技术 |
|---|---|
| Backend | Go |
| RPC | tRPC-Go |
| API | HTTP / WebSocket |
| DB | MySQL |
| Cache | Redis |
| MQ | RocketMQ / Kafka |
| Search | Elasticsearch |
| Vector | Elasticsearch Vector |
| Audio | OSS / S3 |
| Codec | Opus |
| ASR | Streaming ASR |
| Speaker | Speaker Embedding |
| LLM | AI Gateway |
| Image | Image Generation |
| Monitoring | Prometheus |
| Logging | Loki |
| Trace | OpenTelemetry |

---

# 五十一、核心 Service

## API Service

负责：

```text
User
Device
Timeline
Memory
Topic
Project
Todo
Review
Agent
```

---

## Audio Service

负责：

```text
Upload
Chunk
Session
Transcoding
Storage
```

---

## ASR Service

负责：

```text
Streaming ASR
Batch ASR
Punctuation
Diarization
```

---

## Understanding Service

负责：

```text
Event
Todo
Decision
Entity
Topic
Memory
```

---

## Agent Service

负责：

```text
Chat
Planning
Tool Calling
Memory Retrieval
Mutation
Review
```

---

# 五十二、MQ

建议使用 RocketMQ / Kafka。

事件：

```text
audio.uploaded
audio.processed
asr.completed
speaker.detected
memory.created
todo.created
topic.updated
risk.detected
review.requested
```

Pipeline：

```text
AudioUploaded
 ↓
ASR
 ↓
TranscriptCreated
 ↓
SpeakerRecognition
 ↓
SemanticExtraction
 ↓
MemoryCreated
 ↓
TopicUpdate
 ↓
TodoUpdate
 ↓
RiskDetection
```

---

# 五十三、MySQL

核心表：

```text
user
device
device_session

audio_session
audio_chunk

transcript
transcript_segment

speaker
speaker_voiceprint

memory
memory_entity
memory_relation

topic
topic_member
topic_timeline

project
project_task
project_milestone

todo
decision
risk
insight

reminder

daily_review
weekly_review

agent_conversation
agent_message
agent_action

user_preference
```

---

# 五十四、Memory

```text
memory
----------------
id
user_id
type
title
content

source_type
source_id

event_time
created_at
updated_at

importance
confidence

topic_id
project_id

embedding_id
status
```

---

# 五十五、Todo

```text
todo
----------------
id
user_id

title
description

source_memory_id
topic_id
project_id

priority
status

due_at
completed_at

confidence

created_by
created_at
updated_at
```

---

# 五十六、Topic

```text
topic
----------------
id
user_id

name
description
type

goal
status

start_at
target_at

progress
health_score

created_by

created_at
updated_at
```

---

# 五十七、ES

Index：

```text
transcript_index
memory_index
todo_index
topic_index
person_index
```

支持：

```text
BM25
+
Semantic Search
+
Metadata Filter
```

例如：

> “我上个月和 Alice 讨论过招聘的问题。”

检索：

```text
Person = Alice
Time = last month
Topic = Hiring
Semantic Similarity
```

---

# 五十八、Redis

用于：

```text
User Session
Device Connection
Realtime State
Agent Context
Recent Memory
Topic State
Rate Limit
Distributed Lock
Task Dedup
```

---

# 五十九、AI Gateway

业务层不要直接绑定具体模型。

建立统一：

```text
AI Gateway
```

提供：

```text
ASR
LLM
Embedding
TTS
Vision
Image Generation
```

支持：

```text
Model Router
Fallback
Cost Control
Latency Control
A/B Test
Prompt Version
Model Version
```

---

# 六十、AI Pipeline

不要使用一个“大模型 Prompt”解决所有问题。

采用：

```text
Transcript
 ↓
Fast Model
 ↓
Fact Extraction
 ↓
Classifier
 ↓
Memory Candidate
 ↓
Strong Model
 ↓
Reasoning
 ↓
Topic Update
 ↓
Agent
```

---

# 六十一、模型分层

## Tier 1：Realtime

低延迟、低成本。

负责：

```text
Classification
Entity
Todo
Topic
```

## Tier 2：Reasoning

负责：

```text
Risk
Project Progress
Long-term Memory
Daily Review
Weekly Review
```

## Tier 3：Creative

负责：

```text
Comic
Story
Visual Review
```

---

# 六十二、Context Engineering

Agent 不应该把用户所有历史发送给 LLM。

Context：

```text
System
+
Current Conversation
+
Recent Memory
+
Relevant Topic
+
Relevant Person
+
Relevant Todo
+
Long-term Memory
```

采用：

```text
Hybrid Retrieval
```

---

# 六十三、Agent 查询示例

用户：

> “我最近为什么一直没学 Rust？”

检索：

```text
Rust Topic
+
Recent 30d Memory
+
Work Topic
+
Todo
+
Recent Events
```

Agent：

> “从最近 30 天的记录看，主要是工作项目占用了你晚上的时间。过去 7 天你有 4 天提到工作比较忙。”

---

# 六十四、准确率

不同信息采用不同阈值：

| 类型 | Confidence |
|---|---:|
| Transcript | > 0.90 |
| Speaker | > 0.90 |
| Entity | > 0.85 |
| Todo | > 0.85 |
| Topic | > 0.80 |
| Risk | > 0.75 |
| Insight | > 0.65 |

核心原则：

> **低置信度不是事实，而是建议。**

---

# 六十五、核心 AI KPI

## Memory Precision

AI 生成 Memory 中：

> 用户认为有价值的比例。

## Todo Precision

AI 生成 Todo 中：

> 用户确认真正应该做的比例。

## Insight Acceptance

```text
Like
Save
Follow
Dismiss
```

## Correction Rate

用户修改 AI 信息的比例。

## Agent Success Rate

Agent 是否正确完成用户要求。

---

# 六十六、功耗策略

硬件重点：

```text
DSP VAD
+
Low Power MCU
+
Opus
+
Flash Buffer
+
BLE
+
Wi-Fi On Demand
```

不要：

```text
CPU
+
Wi-Fi
+
Cloud Streaming
```

24 小时全开。

---

# 六十七、智能上传

### 高价值

检测：

```text
Human Voice
Conversation
User Speech
Meeting
Semantic Signal
```

立即上传。

### 低价值

```text
Silence
Noise
Music
TV
```

延迟上传或丢弃。

---

# 六十八、成本控制

不能把全天所有音频全部送到最高成本模型。

Pipeline：

```text
VAD
 ↓
ASR
 ↓
Lightweight Classification
 ↓
Information Density
 ↓
Strong LLM
```

只把：

```text
Important Conversation
Todo
Decision
Project
Insight
```

送给高级模型。

---

# 六十九、数据一致性

```text
MySQL
= Source of Truth

ES
= Search Index

Redis
= Cache

OSS
= Audio Source
```

---

# 七十、安全

所有 AI Action 都需要权限控制。

普通：

```text
Read
Search
```

可以自动执行。

高风险：

```text
Delete
Bulk Modify
Export
Share
```

需要用户确认。

---

# 七十一、AI Audit

记录：

```text
model
prompt_version
input_memory_ids
output
confidence
created_at
```

支持：

> “为什么知微会这样判断？”

---

# 七十二、Prompt Registry

Prompt 不应该写死代码。

例如：

```text
todo_extraction_v12
memory_extraction_v8
topic_detection_v4
daily_review_v15
risk_detection_v7
```

支持：

```text
A/B
Rollback
Evaluation
```

---

# 七十三、Evaluation

建立内部标注数据：

```text
Audio
Transcript
Expected Speaker
Expected Memory
Expected Todo
Expected Topic
```

自动评估：

```text
ASR
Speaker
Memory
Todo
Topic
Summary
Agent
```

---

# 七十四、人工标注

建立 Annotation Tool：

```text
Audio
 ↓
Transcript
 ↓
Speaker
 ↓
Memory
 ↓
Todo
 ↓
Topic
```

支持：

```text
Correct
Reject
Merge
Split
```

持续形成训练 / Evaluation Dataset。

---

# 七十五、隐私与合规

由于知微属于：

> **全天候环境录音产品**

隐私与合规必须从 Day 1 进入产品设计。

重点包括：

- 用户明确授权
- 录音状态可感知
- 硬件录音指示
- Private Mode
- 数据加密
- 数据删除
- 数据导出
- 数据保留期限
- 第三方声音数据处理
- 数据访问权限
- 数据跨境策略

具体国家/地区上线前，需要针对录音同意、隐私告知和数据处理义务进行法律评估。

---

# 七十六、MVP

第一版只做最核心闭环。

## Hardware

- 持续录音
- BLE
- Flash
- VAD
- Opus
- 电量
- OTA

## App

- Device Pair
- Timeline
- Transcript
- Speaker
- Memory
- Todo
- Topic
- Agent
- Daily Review

## Cloud

- ASR
- Speaker
- LLM
- Memory
- Search
- Agent

---

# 七十七、MVP 核心用户闭环

```text
戴上知微
 ↓
正常生活
 ↓
自动录音
 ↓
手机自动同步
 ↓
ASR
 ↓
AI 理解
 ↓
Timeline
 ↓
Memory
 ↓
Todo
 ↓
Topic
 ↓
Daily Review
 ↓
问知微
```

---

# 七十八、V1.1

增加：

```text
Voiceprint
Person
Project
Risk
Reminder
Weekly Review
Memory Search
```

---

# 七十九、V1.2

增加：

```text
Topic Auto Discovery
Progress Tracking
Alternative Plans
Personal Insights
Behavior Trends
```

---

# 八十、V2

增加：

```text
Calendar
Email
Task Manager
Health Data
Location
Smartwatch
Vision
External Apps
```

最终成为：

> **Personal Operating System**

---

# 八十一、最终产品模型

```text
                         知微
                          │
                  Personal Memory
                          │
       ┌──────────────────┼──────────────────┐
       │                  │                  │
     People             Topics           Projects
       │                  │                  │
       └──────────────────┼──────────────────┘
                          │
                        Agent
                          │
          ┌───────────────┼───────────────┐
          │               │               │
         Todo            Risk           Insight
          │               │               │
          └───────────────┼───────────────┘
                          │
                        Action
                          │
                        Review
                          │
                        Memory
                          │
                          └──────────→ Loop
```

---

# 八十二、知微的核心护城河

知微真正的壁垒不是：

```text
ASR
LLM
录音硬件
```

这些都可以被复制。

真正的壁垒：

```text
Personal Memory Graph
+
Long-term Context
+
Topic Evolution
+
User Correction
+
Behavior Pattern
+
Agent Action History
```

随着用户使用时间增加：

> **知微越来越懂这个人。**

---

# 八十三、知微的长期产品形态

最终知微可以形成：

```text
知微
│
├── Memory
│
├── Personal CRM
│
├── Personal Todo
│
├── Personal Project Manager
│
├── Personal Journal
│
├── Personal Coach
│
└── Personal Agent
```

不是：

> “一个 AI 录音硬件。”

而是：

> **一个真正持续了解用户的 Personal AI。**

---

# 八十四、产品 North Star

不建议把：

```text
录音时长
DAU
ASR 时长
```

作为最终北极星。

建议：

## Weekly Valuable Memory

> 每周有多少条知微生成的 Memory 被用户查看、确认、引用或用于后续行动。

进一步定义：

## Life Progress Rate

用户通过知微：

```text
发现问题
 ↓
制定计划
 ↓
执行
 ↓
完成
```

的比例。

---

# 八十五、品牌核心表达

## 中文

> **知微，见微知著。**

产品价值：

> **记住生活，看见自己。**

---

## 英文

> **Zhiwei — Understand Your Life.**

或者：

> **Zhiwei — Remember. Understand. Act.**

---

# 八十六、最终产品一句话

> **知微是一款全天候理解你的 AI 生活智能体：它替你记住生活中的每一个重要片段，从微小的信息中发现目标、任务、风险和变化，并持续帮助你把事情推进下去。**

---

# 八十七、产品最终愿景

```text
过去
 ↓
知微帮你记住

现在
 ↓
知微帮你理解

未来
 ↓
知微帮你行动

长期
 ↓
知微帮你认识自己
```

最终：

> **见微知著，知己知行。**

这应该成为「知微」整个产品体系最核心的产品哲学。