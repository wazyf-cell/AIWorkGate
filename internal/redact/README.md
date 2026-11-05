# P10-1 · 脱敏规则层 internal/redact

把 magpie `internal/redact` 的**规则层**移植成 `aiworkgate/internal/redact`（依据 D-23、P10 计划）。
模型辅助层（magpie `internal/pii` + ONNX）**不在本包**，留到 P11。

## 对外 API（三件套 + 扩展）

```go
func Mask(s string, o Options) (string, int)        // 文本：替换 + 条数
func MaskJSON(body []byte, o Options) ([]byte, int) // JSON 体逐字段
func NewWriter(w http.ResponseWriter) *Writer       // SSE/整包响应回程还原（Finish 收尾）
// P10-⑤ 扩展（审计，只记统计，绝不记原文）：
func MaskAudit(s string, o Options) (string, *Audit)
func MaskJSONAudit(body []byte, o Options) ([]byte, *Audit)
// Audit.Records() → {Kind, Count, Line, Elapsed}；无任何字段能携带原文/占位/前缀。
```

- **类别表**（`more.go` Categories）：`ssn/passport/iban/birthday/mac/serial` 默认开；`home/userhost/ip/bucket` 默认关；
  `Options.Kinds` 可逐类覆盖。**Kinds 全集以源码为准（23 个，含 `SECRET/EMAIL/PHONE/ACCOUNT/PERSON/ADDRESS` 等），
  没有 `DATE/URL/ORG`**——后三类是模型侧刻意排除（P11），卡面写法属描述误差。
- **占位符 `{{KIND_指纹}}`**：指纹 = HMAC-SHA256(kind+"\x00"+value, key) 的 base32 前 8 位；
  `SetKeyPath(path)` 持久化 key → 同值同占位、跨请求/跨重启稳定；一值一占位、两值不共占位；`Restore` 可逆对账。
- **Route 感知**：`Options.Route` 指明请求会到哪些供应商；同一文本在同一 Route 保持已发形态
  （`known.go` 的 sentSpans），换 Route 重新脱敏——保护上游 prompt cache。
- **回程还原**：`Restore` / `RestoreJSON` / `NewWriter`（`writer.go` 的 `partialTail` 语义）——
  半个占位符被 SSE 帧切断时持住前半，等后半到齐整体还原，事件不增不减。

## 来源与完整性

引擎文件为**移植**（Lead 批复：唯一第三方依赖 `golang.org/x/net/publicsuffix` 定向引入，
`go get` 最小变更 go.mod/go.sum，未做全仓 tidy）。逐字节移植并保留原文件清单之外的说明：

`model.go more.go ids.go phone.go fieldvalue.go dateword.go known.go rules.go scrub.go shape.go json.go zhname.go writer.go rulememo.go zhdict.txt`
（zhdict.txt 由 magpie 源码自带的 `//go:embed` 随包携带，无需改动）

原单文件 `redact.go`（1819 行）按职责拆成 9 个文件（**拆分前 SHA-256 逐字节核验通过；
拆分只动文件边界与 import，函数/类型/变量声明集合前后 98/98 比对无差异，`gofmt` 干净**）：

| 文件 | 职责 |
| --- | --- |
| `rules_table.go` | 包 doc + Options/covers + rule 结构 + 规则表 + 多语密钥名表 |
| `guard.go` | 判定谓词（notCode/emailAt/inUserinfo/secretValue/…） |
| `mailwords.go` | 邮箱/文件后缀/卡片校检词表与判据（realEmail/luhn/chineseID；publicsuffix 唯一用点） |
| `pool.go` | 占位符池（HMAC 指纹 / SetKeyPath 持久化 / values 20 万淘汰最旧一半） |
| `hit.go` | span/Hit/Value/Hint/Mask/Preview |
| `mask.go` | mask 主流程 / ruleSpans / learnLate |
| `escape.go` | 转义视图 / JSON 字符串内保护 / 数字保护 |
| `apply.go` | rule.matches + apply 替换 / Allow / Actions |
| `restore.go` | Restore / partialTail / braceTail / wordTail（SSE 半帧尾巴） |

## 测试

移植 magpie 测试 54 个文件（`package redact` 自包含者，排除依赖 `claudeseal/pii/testenv` 的三个）+ 我们新增：

| 文件 | 内容 |
| --- | --- |
| `eval_cases_test.go` | magpie 原样：评估用例表（**71 条用例、113 个 Keep「不该脱」值**） |
| `eval_test.go` | 重写：规则层评估驱动。Keep 反例全绿是硬闸；Mask 值规则层可脱的必须脱对，模型专属单独计数 |
| `redact_test.go` | magpie 原样（去掉 `claudeseal` 依赖的一项）：Mask/MaskJSON/Writer 全语义 |
| `audit_test.go` | 审计无原文纪律 + 不过度脱敏证据（行号/色值/版本号/尺寸/端口/gh-noreply 不伤） |
| `stability_test.go` | 占位符同值同占/跨请求稳定/可逆；Route 感知 |
| `writer_half_test.go` | SSE 半帧切断（2 片/3 片）还原不破坏 |

```powershell
go test ./internal/redact/...
go vet ./internal/redact/
```

GUI：`go test -v -run 'TestEvalKeepAntiOvermask|TestEvalMaskRules' ./internal/redact/` 看评估统计。

## 边界（本卡）

- 只写 `internal/redact/`；**不接线 gateway**（接线是下一卡）；不改 magpie 源；
- ONNX / `internal/pii` 模型层留 P11（评估里「模型专属（P11）」列的值由模型层补齐）；
- 第三方依赖：`golang.org/x/net/publicsuffix`（Lead 批准定向引入，go.mod/go.sum 最小变更；
  未做 `go mod tidy`；go.mod/go.sum 其余依赖未动）。