# cansim — Classical CAN 2.0A shared-bus drill

A bit-accurate Go simulation of 2–4 nodes sending classical CAN 2.0A **data
frames** over one wired-AND virtual bus. Every time step emits one physical bus
bit; dominant (`0`) wins over recessive (`1`).

## Run

```sh
go test ./...     # 19 tests, including an independent CRC/encoding reference
go run .          # print the demo bus trace and result records
go run . --confinement scenario.json   # TEC/bus-off teaching mode (see below)
```

## What is modeled

- **Data frame fields**, SOF → 11-bit ID → RTR/IDE/r0 → DLC → 0–8 data bytes →
  CRC-15 → CRC delimiter → ACK slot → ACK delimiter → 7 EOF bits.
- **CRC-15** with the CAN polynomial `x^15 + x^14 + x^10 + x^8 + x^7 + x^4 +
  x^3 + 1`.
- **Bit stuffing** restricted to SOF..CRC: after five consecutive equal bits a
  complement is inserted; crossing field boundaries (SOF/ID, ID/control,
  control/data, data/CRC) and a terminal run inside CRC are handled. The
  receiver performs the inverse **destuffing**, rejecting a wrong/missing
  complement as a stuff error.
- **Non-pre-sorted arbitration.** Nodes enqueue frames at integer bit times;
  frames that become ready together collide on SOF. Each node compares every
  bit it sends with what it samples. A recessive sender that samples dominant
  during ID/RTR **loses arbitration**, stops driving, and **continues
  receiving** (so it can still ACK the winner). Frames are never ordered by ID
  or node ahead of time.
- **Wired-AND / dominance.** The wire level is the AND of every driver.
- **ACK.** Every controller that received the frame with a valid CRC drives the
  ACK slot dominant, including nodes whose acceptance filter rejects the frame
  and nodes that lost arbitration. The sender does not ACK itself.
- **Same ID, different data.** Identical identifiers survive arbitration; the
  first differing data/control bit collides and is raised as a **bit error**
  (never resolved by node identity).
- **Error recovery.** An invalid frame is followed by a 6-bit active error
  flag (other controllers join), an 8-bit error delimiter, the 3-bit
  intermission, then bus idle before automatic retransmission. A frame is
  retried **at most three times**, then reported failed.
- **Bit-error fixtures** (`ReceiverBitFault`) name a concrete receiver/transmitter
  node and an absolute physical wire bit; only that controller's sampled bit is
  changed, never the shared wire level.

Out of scope by design: remote frames, extended (29-bit) identifiers, error
counters / error-passive / bus-off.

## Output records (`Result`)

- `Trace` — per-bit time, wire level, field kind, per-node driven levels,
  injected fault samples, and bit-error mismatch flags.
- `ArbitrationExits` — node, wire bit, ID bit number, attempt.
- `Errors` — detection wire bit, flag wire bit, reason, senders, attempt.
- `Transmissions` — per enqueued frame: attempts (1–3) and `ok`/`failed`.
- `Received` — valid frames seen by each application, marked accepted vs.
  filtered (filtered frames were still ACKed).

## Files

- `frame.go` — frame encoding, CRC-15, stuffing.
- `receiver.go` — streaming destuffing decoder and fixed-field checks.
- `bus.go` — wired-AND bus, arbitration, ACK, error frames, retries.
- `confinement.go` — teaching-mode TEC, bus-off and 128×11 recovery observer.
- `main.go` — demo.
- `scenario.go` — `--confinement scenario.json` loader.
- `bus_test.go` — independent CRC/encoding reference plus scenario tests.
- `confinement_test.go` — TEC/bus-off/recovery tests with independent wire-trace replay.
- `demo_confinement.json` — example confinement scenario.


新增 --confinement scenario.json 教学模式，Scenario 含 Nodes（Name、TEC、可选 RecoverAt、Frames 的 ID/Data/Enqueue）、Faults 与 Limit（1～20000 位）。只加入发送 TEC 与 bus-off：真实失败尝试 +8，成功 -1，仲裁退出不计错误；TEC 到 256 后节点不得发送、接收或 ACK，待发送队列保留。显式恢复请求仅在 bus-off 时启动观察，收到 128 个互不重叠的连续 11 隐性位组后恢复，显性位只打断当前未完成组，已完成组保留。计数来自实际线路含空闲位，恢复后原队列继续；期限结束可返回仍阻塞的状态与未发队列。不宣称完整 ISO 错误主动/被动模型。默认模式保留原行为。
