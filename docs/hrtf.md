# 耳机 HRTF 双耳渲染

## 实现

- 数据集：CIPIC subject 003，1250 个方向、左右耳各 200 taps、44.1 kHz；由 `internal/hrtf/data/cipic_subject_003.hrtf` 嵌入二进制，运行时不联网。
- 布局：`internal/hrtf/layout.go` 把 1/2/6/8/12 声道映射到虚拟扬声器方向（5.1(side)、5.1、7.1、7.1.4）。
- 处理：`internal/hrtf/binaural.go` 逐声道与左右耳 HRIR 卷积后求和，输出双声道；`internal/player/hrtf_stream.go` 负责把源流重采样到 44.1 kHz 并适配 `audio.Decoder`。
- 链路：耳机模式且请求 `sky`/`dolby`/`vivid` 时，`internal/player/policy.go` 选择内置 HRTF 路径；`dolby`/`vivid` 会改用 `sky` 5.1 FLAC（Go 侧不解 E-AC-3/AV3A），`jyeffect` 不重复叠加 HRTF。

## 如何检查

1. 启动应用，打开设置 →「高级音频与硬件解码」→「空间音频模式」，选择「耳机空间化（HRTF 双耳）」。
2. 播放一首 `sky`（沉浸环绕声）或 `dolby`/`vivid`（会自动回退到 sky 5.1 FLAC）的歌曲，打开正在播放页的音质徽章：应看到「HRTF 双耳」和「空间处理：真实 HRTF 双耳卷积（CIPIC 360° HRIR）」。
3. 用耳机听：左右声道应有明显的双耳定位差异，而不是单纯的左右音量平衡；旋转/移动头部不会改变声像（本实现没有头部跟踪，这是预期限制）。
4. 对照测试：把同一首歌切到「立体声（下混）」，HRTF 模式的声场应更宽、外部化更明显；切到「多声道扬声器」时不应走 HRTF 路径。
5. 自动化验证：`go test ./internal/hrtf ./internal/player -run 'HRTF|PlanPlayback|FLAC'`。

## 限制

- 当前没有头部跟踪、个性化 HRTF、SOFA 文件导入 UI；数据集固定为 CIPIC subject 003。
- `jyeffect` 是网易云已空间化的立体声，按既有策略不重复叠加 HRTF。
- DLNA 仍只投送 `jyeffect` 立体声；不把未确认能力的多声道直接投给接收器。
- `ao=null` 和单元测试只能验证处理链与声道能量，不能证明某副耳机/扬声器的实际听感；最终需用真实耳机试听。
