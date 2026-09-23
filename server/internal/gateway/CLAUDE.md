# gateway：节点连接

规格：spec/20。字段以 `panel-spec/proto/node/v1` 为准。

- 握手在首个 `Frame.hello` 中完成（NODE-08–11），与 WSS 或长轮询无关。
- 每条连接一个读协程、一个写协程；持锁期间不做 I/O；所有协程受 context 控制。
- NODE-12–14 至少一次投递 + 幂等；窗口溢出改发全量。
- WSS 与长轮询共用同一套帧、加密、确认逻辑。
- 必须通过 `go test -race` 与 `e2e/conformance`；并发改动使用 concurrency-reviewer。
