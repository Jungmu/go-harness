# Go Harness System Plan

## 1. 문서 목적

이 문서는 `Symphony`의 상위 개념과 이 저장소 루트의 [SPEC.md](SPEC.md)를 기준으로, 개인 또는 소규모 팀이 운영할 수 있는 Go 기반 하네스 시스템의 v1 설계를 정의한다.

목표는 "에이전트를 직접 조작하는 도구"가 아니라, "작업을 안전하고 반복 가능하게 에이전트에게 위임하는 실행 시스템"을 만드는 것이다.

이 문서는 다음 질문에 답해야 한다.

- 어떤 문제를 해결하는가
- v1에서 어디까지 구현하는가
- 어떤 계약을 코드로 고정해야 하는가
- 어떤 순서로 구현하는가

## 2. 정렬 원칙과 의도적 차이

이 문서의 규범 우선순위는 다음과 같다.

1. 이 저장소 루트의 [SPEC.md](SPEC.md)
2. 이 문서 `PLAN.md`
3. upstream Elixir reference implementation

의도는 `SPEC.md`와 최대한 정렬하되, Go v1에서 단순화를 위해 일부 차이를 명시적으로 유지하는 것이다.

### 2.1 Go v1의 의도적 차이

- v1은 `local worker only`다.
- v1은 `no persistent DB`다.
- tracker write surface는 `linear_graphql` 대신 Go 전용 고수준 동적 도구를 사용한다.
- Go v1은 GitHub 연동을 필수로 두고, `Done` 전이 직전에 PR 생성 또는 재사용을 하네스 책임으로 둔다.
- Go v1은 `github.token`이 비어 있으면 startup에서 `gh auth token --hostname <host>`로 GitHub CLI 인증을 재사용할 수 있다.
- HTTP server는 `SPEC.md` 기준으로는 extension이지만, Go v1에서는 권장 기본 observability surface로 둔다.
- Go v1의 기본 배포 경로는 `go install`이 아니라 GitHub Releases의 prebuilt binary다. 1차 지원 타깃은 macOS Apple Silicon, Linux x86_64, Linux ARM64다.

### 2.2 경로 표기 규칙

이 문서 안의 경로는 모두 이 저장소 루트를 기준으로 적는다.

- `WORKFLOW.md`
- `AGENTS.md`
- `SPEC.md`
- `docs/architecture.md`
- `cmd/harnessd/main.go`
- `internal/domain`

`go-harness/...` 같은 자기중첩 표기는 사용하지 않는다.

## 3. 배경과 문제 정의

Harness engineering 관점에서 중요한 점은 다음과 같다.

- 좋은 결과는 모델 성능만으로 나오지 않는다.
- 저장소 구조, 실행 규칙, 검증 루프, 관측성이 함께 설계되어야 한다.
- 오케스트레이터는 만능 비즈니스 로직 엔진이 아니라, 작업을 고립된 환경에서 안정적으로 실행시키는 런타임이어야 한다.

개발자가 반복적으로 겪는 문제는 다음과 같다.

- 이슈를 보고 에이전트에게 작업을 시키는 과정이 매번 수동이다.
- 여러 작업을 동시에 돌리면 로그, 상태, 작업 디렉터리가 섞인다.
- 재시도 기준과 실패 처리 방식이 일관되지 않다.
- 프롬프트와 정책이 코드와 분리되어 있어 변경 이력이 남지 않는다.
- 에이전트가 무엇을 했는지 운영자가 추적하기 어렵다.

이 시스템은 위 문제를 다음 방식으로 해결한다.

- 이슈 트래커를 주기적으로 조회한다.
- 작업 단위마다 고립된 workspace를 만든다.
- 저장소 내 `WORKFLOW.md`를 실행 계약으로 사용한다.
- 에이전트 실행 상태를 메모리 상태, 구조화 로그, 상태 API로 관리한다.
- 실패 시 재시도하고, 이슈 상태가 바뀌면 실행을 중단한다.

## 4. 제품 비전

Go 기반 하네스 시스템은 다음과 같은 운영 경험을 제공해야 한다.

- 운영자는 "지금 어떤 이슈가 돌고 있는지"를 즉시 알 수 있다.
- 에이전트 실행은 이슈별 workspace 안에서만 일어난다.
- prompt, sandbox, hooks, tracker 정책은 저장소 안에서 버전 관리된다.
- 서비스 재시작 후에도 치명적인 상태 불일치 없이 다시 동작한다.
- 초기 버전은 단순하지만, 이후 remote worker, dashboard, compatibility shim으로 확장 가능하다.

## 5. 설계 원칙

1. 작게 시작한다.
v1은 local worker only로 제한한다.

2. 계약은 저장소 안에 둔다.
`WORKFLOW.md`, `AGENTS.md`, 검증 스크립트, workspace hooks를 저장소에 둔다.

3. 오케스트레이터는 상태 관리자다.
이슈 코멘트 같은 ticket mutation은 가능한 한 agent가 수행하되, 기본 상태 전환, persistent progress comment 유지, `Done` 직전 GitHub PR 생성은 harness가 책임진다.

4. DB 없이도 운영 가능해야 한다.
단일 프로세스, 메모리 상태, 파일 기반 로그, 상태 API만으로 시작한다.

5. 관측성을 기본값으로 둔다.
모든 런타임 이벤트는 구조화 로그와 snapshot으로 남긴다.

6. Go답게 단순하게 만든다.
과한 프레임워크보다 `context`, goroutine, channel, `net/http`, `slog`, `os/exec` 중심으로 구성한다.

7. 안전성은 명시적으로 설계한다.
trusted environment 전제를 두더라도 path validation, scope narrowing, timeout, cancellation은 기본 기능으로 넣는다.

## 6. v1 범위

### 6.1 포함

- Linear 기반 issue polling
- `WORKFLOW.md` 로드, 검증, watch/reload
- 필수 GitHub 설정 로드, GitHub CLI auth fallback, PR 생성
- startup validation과 per-tick dispatch validation
- 이슈별 workspace 생성 및 유지
- local machine에서 Codex app-server 실행
- 단일 프로세스 orchestrator
- bounded concurrency
- continuation turn 처리
- 실패 재시도와 exponential backoff
- 이슈 상태 변경 시 실행 중단과 cleanup
- persistent Linear progress comment upsert
- JSON structured logging
- read-only 상태 조회 API
- `POST /api/v1/refresh` 운영 트리거
- 동일 snapshotter를 재사용하는 CLI `status`

### 6.2 제외

- SSH remote workers
- 멀티 트래커 지원
- 다중 사용자, 다중 프로젝트 control plane
- persistent scheduler DB
- 자동 PR merge 또는 land 내장 구현
- `linear_graphql` compatibility shim

## 7. 예상 사용자

- 개인 개발자
- 소규모 팀의 내부 자동화 담당자
- agent-friendly repo를 이미 가지고 있는 엔지니어

전제는 다음과 같다.

- 사용자는 Go 설치 없이도 prebuilt binary로 실행할 수 있고, CLI 운영에는 익숙하다.
- 트래커는 우선 Linear를 사용한다.
- Codex 또는 동등한 app-server compatible runtime을 사용할 수 있다.
- 운영자는 trusted environment 전제를 이해하고 직접 선택할 수 있다.

## 8. 시스템 개요

핵심 컴포넌트는 다음과 같다.

1. Workflow Loader
`WORKFLOW.md`를 찾고, YAML front matter와 prompt body를 분리하고, strict template contract를 검증한다.

2. Config Layer
기본값 적용, `$VAR` 해석, 경로 정규화, 런타임 검증, last-known-good 유지, reload 적용을 담당한다.

3. Tracker Adapter
candidate issue 조회, issue 상태 재조회, normalized issue model 제공, agent-facing write tool의 서버측 실행을 담당한다.

4. GitHub Integration
workspace git 상태 확인, configured token 또는 GitHub CLI credential 로드, issue branch push, GitHub PR 생성 또는 open PR 재사용을 담당한다.

5. Orchestrator
poll loop, concurrency 제어, `claimed`/`running`/`retry` 상태 관리, dispatch, stop, reconcile, `Done` 직전 PR handoff를 담당한다.

6. Workspace Manager
issue별 workspace path 생성, hooks 실행, cleanup, 안전한 cwd 검증을 담당한다.

7. Agent Runner
Codex app-server subprocess 생성, startup handshake, event streaming, continuation turn, timeout과 stall 감지를 담당한다.

8. Agent Tool Surface
app-server 세션에 주입할 tracker write용 동적 도구를 정의하고, 현재 issue와 현재 project 범위 안에서만 mutation이 일어나게 한다.

9. Observability
구조화 로그, 설정 가능한 로그 레벨, 상태 스냅샷, HTTP 상태 surface, 최근 이벤트 버퍼, issue별 timeline history JSONL, 선택적 raw prompt transcript JSONL, token/rate-limit 집계를 제공한다.

## 9. 제안 디렉터리 구조

```text
.
  SPEC.md
  PLAN.md
  WORKFLOW.md
  AGENTS.md
  cmd/
    harnessd/
      main.go
  internal/
    domain/
    workflow/
    config/
    github/
    tracker/
      linear/
    workspace/
    agent/
      codex/
      tools/
    orchestrator/
    observability/
    server/
  docs/
    architecture.md
    workflow-contract.md
  examples/
```

설계 의도는 다음과 같다.

- `cmd/harnessd`는 실제 데몬 엔트리포인트다.
- `internal/domain`은 normalized type과 DTO를 정의한다.
- `internal/orchestrator`는 서비스의 중심 상태 기계다.
- `internal/github`는 workspace를 GitHub PR handoff로 연결한다.
- `internal/agent/codex`는 app-server adapter다.
- `internal/agent/tools`는 app-server에 주입할 동적 도구 구현이다.
- `internal/server`는 `/`, `/healthz`, `/api/v1/*`를 제공한다.
- `internal/observability`는 로그 필드 표준화와 snapshotter를 담당한다.

## 10. 핵심 도메인 모델과 정규화 규칙

### 10.1 Issue

- `ID`
- `Identifier`
- `Title`
- `Description`
- `Priority`
- `State`
- `BranchName`
- `URL`
- `Labels`
- `BlockedBy`
- `CreatedAt`
- `UpdatedAt`

### 10.2 WorkflowDefinition

- `SourcePath string`
- `Config map[string]any`
- `PromptTemplate string`

### 10.3 RuntimeConfig

- polling interval
- workspace root
- hook scripts and timeout
- active states
- terminal states
- max concurrent agents
- optional per-state concurrency caps
- retry backoff
- codex command and timeout settings
- approval and sandbox settings
- server port

### 10.4 Workspace

- `Path`
- `WorkspaceKey`
- `CreatedNow`

### 10.5 RunAttempt

- `IssueID`
- `Identifier`
- `Attempt`
- `WorkspacePath`
- `StartedAt`
- `Status`
- `Error`

### 10.6 LiveSession

- `SessionID`
- `ThreadID`
- `TurnID`
- `StartedAt`
- `LastEvent`
- `LastEventAt`
- `LastMessage`
- `InputTokens`
- `OutputTokens`
- `TotalTokens`
- `TurnCount`
- `AppServerPID`
- `Worker`

### 10.7 RecentEvent

- `At`
- `Event`
- `Message`
- `PayloadSummary`

### 10.8 RetryEntry

- `IssueID`
- `Identifier`
- `Attempt`
- `DueAt`
- `Reason`
- `TimerRef`

### 10.9 RuntimeTotals

- `InputTokens`
- `OutputTokens`
- `TotalTokens`
- `SecondsRunning`

### 10.10 RateLimitSnapshot

- `Provider`
- `UpdatedAt`
- `Raw map[string]any`

### 10.11 RunningEntry

- `Issue`
- `Attempt`
- `WorkspacePath`
- `Worker`
- `StartedAt`
- `Cancel`
- `LiveSession`
- `RecentEvents`
- `LastError`

### 10.12 RuntimeState

- `Running map[string]RunningEntry`
- `Claimed map[string]struct{}`
- `RetryQueue map[string]RetryEntry`
- `Completed map[string]struct{}`
- `PollInterval`
- `MaxConcurrentAgents`
- `NextPollAt`
- `PollInProgress`
- `CodexTotals`
- `CodexRateLimits`

상태 모델 원칙은 다음과 같다.

- `RuntimeState`는 orchestrator의 authoritative in-memory state다.
- `RunningEntry`는 실행 제어용 상태다.
- `LiveSession`은 observability와 status API용 상태다.
- `RetryEntry`는 retry scheduling과 backoff 계산의 단일 기준이다.
- `Completed`는 bookkeeping 용도이며 dispatch gating에는 쓰지 않는다.
- `RecentEvents`는 issue별 최근 50개 구조화 이벤트만 유지해 메모리 사용을 제한한다.
- status API는 내부 상태를 그대로 노출하지 않고 외부용 DTO로 투영한다.

정규화 규칙은 다음과 같다.

- `WorkspaceKey`는 `issue.identifier`에서 `[A-Za-z0-9._-]` 외 문자를 모두 `_`로 치환한 값이다.
- issue state 비교는 lowercase 정규화를 사용한다.
- `SessionID`는 `<thread_id>-<turn_id>` 형식으로 조합한다.

## 11. 오케스트레이션 상태 머신과 실행 흐름

### 11.1 내부 orchestration 상태

트래커 상태와 별개로, 서비스 내부 상태는 다음 다섯 가지다.

1. `Unclaimed`
이슈가 실행 중이 아니고 retry도 예약되어 있지 않다.

2. `Claimed`
오케스트레이터가 중복 dispatch를 막기 위해 이슈를 예약했다.

3. `Running`
worker가 존재하고 `running` map에 들어 있다.

4. `RetryQueued`
worker는 멈췄지만 retry timer가 존재한다.

5. `Released`
claim이 해제되었고 다시 poll 결과에 따라 재판단된다.

### 11.2 서비스 시작 흐름

1. logger와 observability sink를 초기화한다.
2. `WORKFLOW.md`를 찾고 로드한다.
3. 같은 디렉터리에 `REVIEW-WORKFLOW.md`가 있으면 함께 로드한다.
4. startup validation을 수행한다.
5. `github.token`이 없으면 `github.endpoint`에서 host를 계산해 `gh auth token --hostname <host>` warmup을 수행한다.
6. invalid config거나 auth warmup이 실패하면 서비스 시작을 거부한다.
7. last-known-good config를 메모리에 적재한다.
8. coding lane만 terminal issue cleanup을 1회 수행한다.
9. workflow file watch를 시작한다.
10. coding orchestrator와 optional review orchestrator를 시작한다.
11. aggregate snapshot을 노출하는 optional HTTP server를 시작한다.

### 11.3 정상 dispatch 흐름

1. poll tick이 시작된다.
2. per-tick dispatch validation을 수행한다.
3. validation이 실패하면 신규 dispatch는 건너뛰고 reconciliation만 수행한다.
4. Linear에서 candidate issues를 조회한다.
5. candidate issue는 `priority` 오름차순, `created_at` 오름차순, `identifier` 오름차순으로 정렬한 뒤 현재 `claimed`, `running`, `retry` 상태를 고려해 dispatch 대상을 고른다.
6. issue별 workspace를 준비하고 hook을 실행한다.
7. dispatch 시작 시 issue를 `In Progress`로 전환한다.
8. Codex app-server session을 시작한다.
9. 첫 turn에는 전체 rendered prompt를 보낸다.
10. runner는 이벤트를 스트리밍하고 `LiveSession`, `RecentEvents`, aggregate totals를 갱신한다.
11. `turn/completed` 후 issue가 active면 같은 thread에서 continuation turn을 시작한다.
12. run이 성공적으로 끝나고 explicit retry stop reason이 없으면 workspace branch를 push하고 GitHub PR을 생성 또는 재사용한다.
13. PR handoff가 성공하면 issue를 `Done`으로 전환한다.
14. PR handoff가 실패하면 attempt failure로 기록하고 retry policy를 적용한다.
15. issue가 terminal이면 session을 중단하고 workspace를 정리한다.
16. issue가 non-active non-terminal이면 session만 중단하고 cleanup은 정책에 따라 분리한다.

### 11.3.a review dispatch 흐름

1. review lane은 `REVIEW-WORKFLOW.md`가 있을 때만 활성화된다.
2. review lane은 `tracker.active_states = ["In Review"]` candidate만 조회한다.
3. review lane은 workspace를 준비한 뒤 stale `.harness/review-result.json`을 지운다.
4. review lane은 `REVIEW-WORKFLOW.md` body를 렌더링하고 internal review contract suffix를 덧붙인다.
5. review lane은 continuation 없이 정확히 한 turn만 실행한다.
6. review turn이 성공하면 `.harness/review-result.json`과 `.harness/review-notes.md`를 검증한다.
7. verdict가 `done`이면 workspace branch를 push하고 GitHub PR을 생성 또는 재사용한 뒤 issue를 `Done`으로 전환하고 workspace를 정리한다.
8. verdict가 `todo`이면 issue를 `Todo`로 전환하고 claim만 해제한다. workspace는 유지한다.
9. review artifact가 없거나 invalid면 attempt failure로 처리하고 normal retry policy를 적용한다.

### 11.4 continuation 규칙

- 하나의 run 안에서는 동일 `thread_id`를 재사용한다.
- continuation turn은 새 subprocess를 만들지 않고 기존 app-server 세션을 유지한다.
- 첫 turn은 전체 task prompt를 사용한다.
- 이후 turn은 continuation guidance만 보낸다.
- `turn/completed` 자체는 곧 issue 완료를 뜻하지 않지만, run이 정상 종료되고 issue가 여전히 active면 harness가 GitHub PR handoff 후 `Done` 전환을 시도한다.
- issue가 여전히 active이고 `agent.max_turns` 미만이면 같은 session에서 다음 turn을 시작한다.
- issue가 active인데 `agent.max_turns`에 도달하면 현재 run을 종료하고 issue를 `In Review`로 전환한다.
- review lane은 continuation을 사용하지 않는다.
- coding lane workspace에 `.harness/review-notes.md`가 있으면 prompt suffix로 먼저 읽도록 지시한다.

### 11.5 reconciliation과 cancellation 규칙

- orchestrator만 `claimed`, `running`, `retry` 상태를 변경할 수 있다.
- runner는 종료 사유와 최신 이벤트를 돌려주지만 dispatch 여부는 결정하지 않는다.
- poll 시점과 turn 종료 시점 모두에서 issue의 active 또는 terminal 여부를 다시 확인한다.
- running issue가 terminal로 바뀌면 session을 취소하고 cleanup을 수행한다.
- running issue가 active가 아닌 다른 상태로 바뀌면 session을 취소하고 claim을 해제한다.
- running 또는 retry issue가 tracker refresh에서 사라지면 claim을 해제하고 operator-visible timeline event를 남긴다.
- retry queue는 실패 후 재실행과 정상 turn 후 continuation 둘 다 표현할 수 있어야 한다.

## 12. `WORKFLOW.md`와 설정 계약

### 12.1 파일 탐색과 경로 해석

workflow file path precedence는 다음과 같다.

1. explicit runtime setting 또는 CLI startup path
2. 현재 프로세스 working directory의 `WORKFLOW.md`
3. 실행 파일 디렉터리의 `WORKFLOW.md`

loader 동작은 다음과 같다.

- 파일을 읽지 못하면 `missing_workflow_file` 오류를 반환한다.
- workflow file은 저장소 소유 설정이며 version-controlled resource로 간주한다.
- 같은 디렉터리에 `REVIEW-WORKFLOW.md`가 있으면 review lane이 그 파일을 별도 설정/프롬프트 계약으로 사용한다.

### 12.2 파일 형식과 파싱 규칙

`WORKFLOW.md`는 optional YAML front matter를 가진 Markdown 파일이다.

파싱 규칙은 다음과 같다.

- 파일이 `---`로 시작하면 다음 `---`까지를 YAML front matter로 파싱한다.
- 나머지 본문은 prompt body가 된다.
- front matter가 없으면 전체 파일을 prompt body로 보고 config는 빈 map으로 간주한다.
- YAML front matter는 반드시 map으로 decode되어야 한다.
- map이 아닌 YAML은 `workflow_front_matter_not_a_map` 오류다.
- prompt body는 trim한 뒤 사용한다.

### 12.3 top-level schema

현재 인식하는 top-level key는 다음과 같다.

- core: `tracker`, `polling`, `workspace`, `hooks`, `agent`, `codex`
- extension: `server`

unknown key는 forward compatibility를 위해 무시한다.

### 12.4 front matter 필드와 기본값

#### `tracker`

- `kind`
  - 필수
  - 현재 지원 값은 `linear`
- `endpoint`
  - 기본값: `https://api.linear.app/graphql`
- `api_key`
  - literal token 또는 `$VAR_NAME`
  - canonical env는 `LINEAR_API_KEY`
  - `$VAR_NAME`은 process environment를 먼저 보고, 없으면 실행 파일 디렉터리의 `.env`를 fallback으로 본다
  - `$VAR_NAME`이 빈 문자열로 해석되면 missing으로 본다
- `project_slug`
  - `tracker.kind == "linear"`일 때 dispatch에 필수
  - Linear project `slugId` 또는 exact project name을 허용한다
- `active_states`
  - 기본값: `["Todo", "In Progress"]`
- `terminal_states`
  - 기본값: `["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]`

#### `polling`

- `interval_ms`
  - 기본값: `30000`
  - reload 후 future tick scheduling에 즉시 적용한다

#### `workspace`

- `root`
  - 기본값: `<system-temp>/symphony_workspaces`
  - `~`와 `$VAR` 지원
  - `$VAR`는 process environment 우선, 없으면 실행 파일 디렉터리의 `.env`를 사용한다
  - relative root는 허용하지만 권장하지 않는다
  - 런타임에서는 절대경로로 정규화해 사용한다

#### `hooks`

- `after_create`
- `before_run`
- `after_run`
- `before_remove`
- `timeout_ms`
  - 기본값: `60000`

#### `agent`

- `max_concurrent_agents`
  - 기본값: `10`
- `max_turns`
  - 기본값: `20`
- `max_retry_backoff_ms`
  - 기본값: `300000`
- `max_concurrent_agents_by_state`
  - 기본값: 빈 map
  - state key는 lowercase 정규화로 lookup한다

#### `codex`

- `command`
  - 기본값: `"codex app-server"`
- `approval_policy`
  - 기본값: implementation-defined
  - Codex app-server가 지원하는 값을 pass-through로 취급한다
- `thread_sandbox`
  - 기본값: implementation-defined
- `turn_sandbox_policy`
  - 기본값: implementation-defined
- `turn_timeout_ms`
  - 기본값: `3600000`
- `read_timeout_ms`
  - 기본값: `5000`
- `stall_timeout_ms`
  - 기본값: `300000`
  - `<= 0`이면 stall detection을 끈다

#### `server`

- `port`
  - optional extension
  - 지정되면 HTTP server를 시작한다
  - `0`은 ephemeral bind를 의미한다
  - CLI `--port`가 생기면 `server.port`보다 우선한다

### 12.5 값 해석 규칙

- `$VAR` indirection은 secret 또는 path 값에서만 사용한다.
- path 값은 `~` expansion을 허용한다.
- URI나 arbitrary shell command string은 로더가 다시 쓰지 않는다.
- `codex.command`는 shell command string 그대로 유지하고 실제 expansion은 `bash -lc`에 맡긴다.
- `turn_sandbox_policy`는 workspace 경로가 필요한 필드를 런타임 시점에 최종 해석한다.

### 12.6 prompt template 계약

`WORKFLOW.md` body는 per-issue prompt template이다.

rendering 요구사항은 다음과 같다.

- strict template engine을 사용한다.
- unknown variable은 render error다.
- unknown filter는 render error다.
- 최소한 `{{ ... }}` 출력과 `{% if ... %}` 블록을 지원하는 Liquid 또는 Jinja 유사 문법을 지원한다.
- v1은 Go `text/template`로 시작하지 않는다.

template input은 다음과 같다.

- `issue`
  - normalized issue fields 전체
- `attempt`
  - 첫 시도에서는 `null` 또는 absent
  - retry 또는 continuation에서는 integer

fallback 규칙은 다음과 같다.

- prompt body가 비어 있으면 minimal default prompt를 사용할 수 있다.
- 기본 prompt 문구는 `You are working on an issue from Linear.` 로 고정한다.
- workflow read 또는 parse 실패는 prompt fallback으로 숨기지 않는다.

### 12.7 검증과 오류 표면

오류 클래스는 다음을 최소로 가진다.

- `missing_workflow_file`
- `workflow_parse_error`
- `workflow_front_matter_not_a_map`
- `template_parse_error`
- `template_render_error`

startup validation은 다음을 검사한다.

- workflow file read/parse 성공
- `tracker.kind` 존재와 지원 여부
- `tracker.api_key` 존재 여부
- `tracker.project_slug` 존재 여부
- `codex.command` 존재 여부

startup validation이 실패하면 서비스 시작을 거부한다.

per-tick dispatch validation은 같은 검사를 dispatch 직전에 다시 수행한다.

- 실패하면 신규 dispatch는 건너뛴다.
- reconciliation과 cancellation은 계속 수행한다.
- operator-visible error를 남긴다.

### 12.8 reload semantics

reload는 v1 필수 기능이다.

- 시스템은 `WORKFLOW.md` 변경을 watch해야 한다.
- 변경 시 workflow config와 prompt template을 다시 읽고 적용한다.
- 실행 파일 디렉터리의 `.env` 변경도 config reload 대상으로 본다.
- polling cadence, concurrency, active 또는 terminal states, workspace root, hooks, prompt, codex launch config는 future dispatch에 즉시 적용한다.
- in-flight session은 자동 재시작하지 않는다.
- filesystem watch를 놓친 경우를 대비해 dispatch 전에도 방어적으로 reload 또는 re-validate한다.
- invalid reload는 서비스를 죽이지 않는다.
- invalid reload 시 last-known-good effective config를 유지한다.
- invalid reload 상태에서는 신규 dispatch를 막고 operator-visible error를 남긴다.
- `server.port` 같은 listener 설정은 restart-required로 두어도 된다.

### 12.9 `WORKFLOW.md` 예시

```md
---
tracker:
  kind: linear
  project_slug: "my-project"
  api_key: $LINEAR_API_KEY
github:
  token: $GITHUB_TOKEN
  owner: "me"
  repo: "my-repo"
  base_branch: "main"
polling:
  interval_ms: 30000
workspace:
  root: ~/workspaces/go-harness
hooks:
  timeout_ms: 60000
  after_create: |
    test -n "$HARNESS_SOURCE_REPO" || { echo "HARNESS_SOURCE_REPO is required"; exit 1; }
    git -C "$HARNESS_SOURCE_REPO" worktree add --detach "$PWD" main
  before_run: |
    git status --short
  before_remove: |
    test -n "$HARNESS_SOURCE_REPO" || exit 0
    git -C "$HARNESS_SOURCE_REPO" worktree remove --force "$PWD"
agent:
  max_concurrent_agents: 10
  max_turns: 20
  max_retry_backoff_ms: 300000
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
server:
  port: 8080
---

You are working on Linear issue {{ issue.identifier }}.

Title: {{ issue.title }}

{% if issue.description %}
Description:
{{ issue.description }}
{% endif %}
```

## 13. Agent Runner와 app-server 통합 계약

### 13.1 launch contract

subprocess launch 규칙은 다음과 같다.

- command: `codex.command`
- invocation: `bash -lc <codex.command>`
- cwd: issue workspace absolute path
- stdout: line-delimited protocol stream
- stderr: diagnostic stream only

추가 원칙은 다음과 같다.

- stdout만 protocol parsing 대상이다.
- partial stdout line은 newline이 올 때까지 buffer한다.
- complete line이 생기면 JSON parse를 시도한다.
- stderr는 log로 남길 수 있지만 protocol JSON으로 파싱하지 않는다.
- 안전한 buffer를 위해 max line size는 10 MB를 권장한다.

### 13.2 startup handshake

client는 다음 메시지를 순서대로 보낸다.

1. `initialize`
2. `initialized`
3. `thread/start`
4. `turn/start`

각 단계의 의미는 다음과 같다.

- `initialize`
  - `clientInfo`와 `capabilities`를 보낸다
  - dynamic tool advertisement에 필요한 capability가 있으면 여기서 협상한다
  - 응답은 `read_timeout_ms` 안에 받아야 한다
- `initialized`
  - initialization completion notification
- `thread/start`
  - `approvalPolicy`
  - `sandbox`
  - `cwd`
  - supported tool spec
- `turn/start`
  - `threadId`
  - `input`
  - `cwd`
  - `title`
  - `approvalPolicy`
  - `sandboxPolicy`

session 식별 규칙은 다음과 같다.

- `thread_id`는 `thread/start` 결과에서 읽는다.
- `turn_id`는 각 `turn/start` 결과에서 읽는다.
- `SessionID`는 `<thread_id>-<turn_id>`로 만든다.

### 13.3 turn 처리와 종료 조건

turn 종료 조건은 다음과 같다.

- `turn/completed` -> success
- `turn/failed` -> failure
- `turn/cancelled` -> failure
- `turn_timeout_ms` 초과 -> failure
- subprocess exit -> failure
- `user-input-required` -> failure

continuation 규칙은 다음과 같다.

- success 후 issue가 active면 같은 live process와 같은 `thread_id`로 다음 `turn/start`를 호출한다.
- continuation turn에는 전체 task prompt를 재전송하지 않는다.
- app-server subprocess는 run이 끝날 때까지 유지한다.

### 13.4 emitted runtime events

runner는 최소 다음 이벤트를 상위 orchestrator에 전달한다.

- `session_started`
- `startup_failed`
- `turn_completed`
- `turn_failed`
- `turn_cancelled`
- `turn_ended_with_error`
- `turn_input_required`
- `approval_auto_approved`
- `unsupported_tool_call`
- `notification`
- `other_message`
- `malformed`

각 이벤트에는 다음 정보가 포함되어야 한다.

- `event`
- `timestamp`
- `codex_app_server_pid`
- optional `usage`
- optional payload summary

이 이벤트는 다음 용도로 사용한다.

- `LiveSession` 갱신
- issue별 `RecentEvents` 버퍼 갱신
- `CodexTotals` 집계
- `CodexRateLimits` 최신값 반영
- 구조화 로그 출력

### 13.5 approval, user input, tool call 정책

정책은 다음과 같다.

- trusted environment를 전제로 한다.
- approval과 sandbox 값은 configured Codex policy를 pass-through로 사용한다.
- v1은 operator approval relay를 구현하지 않는다.
- `user-input-required`는 unattended workflow와 충돌하므로 hard failure로 처리한다.
- 지원하는 dynamic tool은 session startup 시 advertised tool spec으로 노출한다.
- 지원하지 않는 dynamic tool call은 tool failure response를 반환하고 session은 계속 진행한다.

## 14. Tracker Adapter와 write tool surface

### 14.1 tracker read 경로

- tracker read는 orchestrator 내부 Linear client가 담당한다.
- candidate issue polling, issue refresh, relation lookup, terminal cleanup은 모두 내부 adapter에서 수행한다.
- agent는 직접 tracker credential을 다루지 않는다.

### 14.2 Go v1의 고수준 tracker write 도구

Go v1은 `linear_graphql` 대신 다음 고수준 동적 도구를 app-server session에 주입한다.

#### `tracker_get_issue`

- 인자 없음
- 현재 issue의 normalized snapshot을 반환한다

#### `tracker_list_comments`

- 인자 없음
- 현재 issue의 active comment 목록을 반환한다

#### `tracker_upsert_workpad`

- 인자: `body`
- 현재 issue에서 단일 persistent `## Codex Workpad` comment를 생성 또는 수정한다
- 반환값에는 `comment_id`와 `created` 여부가 포함된다

#### `tracker_transition_state`

- 인자: `state`, optional `note`
- 현재 issue 상태를 변경한다
- 지정한 상태가 workflow 또는 project 정책에 없으면 실패한다

#### `tracker_attach_link`

- 인자: `url`, `title`
- 현재 issue에 attachment 또는 link를 추가한다

#### `tracker_create_followup`

- 인자: `title`, `description`, optional `depends_on_current`
- 현재 issue와 같은 project에 follow-up issue를 만든다
- 생성된 issue는 항상 현재 issue와 `related` relation으로 연결한다
- `depends_on_current=true`면 follow-up issue에 `blockedBy=current issue`를 추가한다

### 14.3 scope restriction

이 도구들은 기본적으로 현재 run 범위 밖의 mutation을 허용하지 않는다.

- arbitrary `issue_id` 입력은 받지 않는다
- arbitrary `project_id` 입력은 받지 않는다
- follow-up issue는 현재 issue와 같은 project에만 생성한다
- cross-project mutation은 reject한다
- tool error는 코드와 메시지를 가진 typed JSON으로 반환한다

### 14.4 compatibility note

이 결정은 `SPEC.md`의 standardized `linear_graphql` extension과 의도적으로 다르다.

- 이유는 v1에서 scope narrowing과 사용성을 우선하기 위해서다.
- 향후 v2에서는 compatibility shim 또는 `linear_graphql` 직접 지원을 검토한다.

## 15. 관측성, 상태 표면, 운영 API

### 15.1 구조화 로그

최소 로그 요구사항은 다음과 같다.

- 모든 주요 이벤트를 JSON 로그로 출력한다.
- `issue_id`, `issue_identifier`, `attempt`, `workspace_path`, `session_id`를 포함한다.
- 에러는 원인과 단계가 드러나야 한다.
- secret과 token은 redaction한다.
- hook output과 stderr는 truncate해서 기록한다.

대표 로그 이벤트는 다음과 같다.

- `poll_started`
- `poll_finished`
- `issue_claimed`
- `workspace_created`
- `agent_started`
- `agent_update`
- `agent_completed`
- `retry_scheduled`
- `issue_released`
- `issue_cancelled`

### 15.2 HTTP surface

Go v1에서 권장하는 status surface는 다음과 같다.

- `/`
  - optional human-readable dashboard
- `GET /healthz`
- `GET /api/v1/state`
- `GET /api/v1/issues/{identifier}`
- `POST /api/v1/refresh`

원칙은 다음과 같다.

- dashboard와 API는 observability 또는 control surface일 뿐, orchestrator correctness에 필수여서는 안 된다.
- unsupported method는 `405 Method Not Allowed`를 반환한다.
- API 오류는 `{"error":{"code":"...","message":"..."}}` envelope를 사용한다.

### 15.3 `GET /api/v1/state`

최소 응답 필드는 다음과 같다.

- `generated_at`
- `counts.running`
- `counts.retrying`
- `running[]`
  - `issue_id`
  - `issue_identifier`
  - `state`
  - `session_id`
  - `turn_count`
  - `last_event`
  - `last_message`
  - `started_at`
  - `last_event_at`
  - `tokens`
- `retrying[]`
  - `issue_id`
  - `issue_identifier`
  - `attempt`
  - `due_at`
  - `error`
- `codex_totals`
- `rate_limits`

### 15.4 `GET /api/v1/issues/{identifier}`

이 endpoint는 issue별 runtime debug view를 반환한다.

최소 포함 정보는 다음과 같다.

- `issue_identifier`
- `issue_id`
- `status`
- `workspace.path`
- `attempts`
- `running`
- `retry`
- `history`

현재 in-memory state에 없는 issue면 `404`를 반환한다.

### 15.5 `POST /api/v1/refresh`

- best-effort로 immediate poll과 reconciliation을 큐잉한다
- repeated request는 coalesce할 수 있다
- 성공 시 `202 Accepted`를 반환한다

### 15.6 CLI `status`

CLI `status`는 별도 상태 모델을 만들지 않는다.

- HTTP surface와 같은 snapshotter를 재사용한다
- 출력만 CLI 친화적으로 포맷한다

## 16. 실패 모델과 재시작 복구

### 16.1 failure classes

1. Workflow 또는 Config Failure
- missing `WORKFLOW.md`
- invalid YAML
- unsupported tracker kind
- missing tracker credential
- missing `codex.command`

2. Workspace Failure
- workspace directory create failure
- workspace population failure
- invalid workspace path
- hook timeout 또는 hook failure

3. Agent Session Failure
- startup handshake failure
- turn failed 또는 cancelled
- turn timeout
- user input requested
- subprocess exit
- stalled session

4. Tracker Failure
- transport error
- non-200 status
- GraphQL error
- malformed payload

5. Observability Failure
- snapshot render failure
- dashboard render failure
- log sink failure

### 16.2 recovery behavior

- dispatch validation failure
  - 신규 dispatch를 건너뛴다
  - 서비스는 계속 산다
  - reconciliation은 계속 수행한다
- workspace 또는 agent failure
  - exponential backoff로 retry queue에 넣는다
- tracker polling failure
  - 해당 tick만 건너뛴다
  - 다음 tick에서 다시 시도한다
- tracker refresh failure
  - 현재 worker는 유지한다
  - 다음 reconciliation에서 다시 확인한다
- observability failure
  - orchestrator를 죽이지 않는다

### 16.3 restart semantics

v1의 scheduler state는 intentionally in-memory다.

재시작 후 동작은 다음과 같다.

- retry timer는 복원하지 않는다
- running session은 복구 시도하지 않는다
- service는 active issue를 다시 poll해서 재판단한다
- startup 시 terminal issue cleanup을 1회 수행한다

이 설계의 tradeoff는 다음과 같다.

- 정확한 "exactly once" 실행 보장은 없다
- 재시작 직후 일부 issue가 재시도될 수 있다
- 이전 프로세스의 live session 메타데이터는 상태 API에 복원되지 않는다

## 17. 보안과 운영 안전성

### 17.1 trust boundary

이 시스템은 trusted environment 전제를 둔다.

- 운영자는 실행 머신과 저장소를 신뢰한다
- hook command는 저장소 소유자가 관리한다
- Codex approval과 sandbox posture는 deployment가 결정한다

### 17.2 filesystem safety

다음은 v1에서 필수다.

- workspace path는 configured workspace root 아래에 있어야 한다
- workspace directory name은 sanitized `WorkspaceKey`를 사용한다
- app-server cwd는 현재 issue의 workspace path여야 한다
- workspace root 바깥 경로 실행은 금지한다
- symlink escape를 차단한다
- 강제 종료 가능한 process handle을 유지한다

### 17.3 secret handling

- `$VAR` indirection을 지원한다
- API token과 secret env 값은 로그에 남기지 않는다
- secret 존재 여부는 검증하되 값을 출력하지 않는다

### 17.4 hook safety

- hooks는 arbitrary shell script이며 trusted config로 간주한다
- hook은 workspace 디렉터리 안에서 실행한다
- hook output은 truncate한다
- hook timeout은 필수다

### 17.5 HTTP와 tracker scope hardening

- HTTP server는 loopback bind를 기본으로 한다
- `server.port=0`은 로컬 개발과 테스트에서 허용한다
- tracker write surface는 현재 issue와 현재 project 범위로 좁힌다
- cross-project mutation은 허용하지 않는다

권장 추가 하드닝은 다음과 같다.

- dedicated OS user로 실행
- workspace root permission 최소화
- network restriction 또는 외부 sandbox 사용
- tool surface와 credential 범위를 최소화

## 18. 기술 선택 초안

- Go 1.24+
- `log/slog`
- `net/http`
- `context`
- `os/exec`
- `gopkg.in/yaml.v3`
- strict Liquid 또는 Jinja compatible template engine

템플릿 엔진 선택 기준은 다음과 같다.

- strict variable mode를 지원해야 한다
- issue object 렌더링이 단순해야 한다
- 문법 복잡도는 가능한 한 낮아야 한다
- 기존 Symphony `WORKFLOW.md` 문법과 최대한 호환되어야 한다

## 19. 구현 우선순위

### Phase 1. 문서와 계약 고정

- `PLAN.md` 개정
- `WORKFLOW.md` 초안
- `AGENTS.md` 초안
- `docs/workflow-contract.md`

### Phase 2. 부팅 가능한 골격

- `go.mod`
- `cmd/harnessd/main.go`
- workflow loader
- config layer
- startup validation
- reload watcher와 last-known-good 보관

### Phase 3. 도메인과 tracker read

- issue model
- runtime state model
- Linear client
- candidate issue fetch
- issue refresh와 normalization

### Phase 4. orchestrator core

- poll loop
- claim, running, retry state machine
- dispatch
- cancellation
- reconciliation
- retry scheduling

### Phase 5. workspace, agent runner, tracker write tools

- workspace create 또는 remove
- hook 실행과 timeout
- Codex app-server launch
- startup handshake
- continuation turn 처리
- dynamic tool 주입
- high-level tracker write tool 구현

### Phase 6. observability와 status surface

- structured log 필드 정리
- snapshotter
- `GET /healthz`
- `GET /api/v1/state`
- `GET /api/v1/issues/{identifier}`
- `POST /api/v1/refresh`
- optional `/` dashboard
- CLI `status`

### Phase 7. 운영 하드닝과 후속 확장

- graceful shutdown
- signal handling
- workspace safety hardening
- remote worker 설계 준비
- `linear_graphql` compatibility shim 검토

## 20. 테스트 계획

### 20.1 workflow loader

- missing file
- invalid YAML
- non-map front matter
- strict template parse failure
- strict template render failure
- valid reload
- invalid reload에서 last-known-good 유지

### 20.2 config와 runtime

- default 값 적용
- `$VAR` 해석
- path normalization
- `codex.command` shell string 유지
- per-tick dispatch validation failure 시 dispatch만 중단

### 20.3 orchestrator

- claimed, running, retry state transition
- active -> terminal 변경 시 cancellation과 cleanup
- active -> non-active 변경 시 release
- continuation turn reuse
- `max_turns_reached` handoff to `In Review`
- restart 후 fresh poll recovery

### 20.4 app-server

- startup handshake 순서
- stdout-only protocol parsing
- partial line buffering
- stalled session timeout
- `user-input-required` hard failure
- unsupported tool call failure-and-continue

### 20.5 tracker tools

- workpad upsert
- state transition
- link attachment
- follow-up creation scope
- unauthorized 또는 wrong-scope request rejection

### 20.6 status surface

- `GET /api/v1/state`
- `GET /api/v1/issues/{identifier}`
- `POST /api/v1/refresh`
- `404` JSON error envelope
- `405` JSON error envelope

## 21. 리스크

1. Codex app-server 프로토콜 결합도
이벤트 payload가 바뀌면 runner가 쉽게 깨질 수 있다.

2. 템플릿 엔진 선택
기존 workflow와 비호환이면 migration 비용이 커진다.

3. DB 없는 retry 모델
재시작 시 retry timer가 사라진다.

4. tracker state mismatch
외부 상태 변경과 local runtime이 어긋날 수 있다.

5. hook 실패
clone 또는 setup 실패가 전체 실행 실패의 큰 비중을 차지할 수 있다.

6. tracker write surface 설계
고수준 도구가 너무 좁으면 workflow 표현력이 부족하고, 너무 넓으면 scope narrowing이 약해진다.

7. 상태 모델 부족
live session, retry, rate-limit, recent event 스키마가 약하면 status API와 graceful shutdown이 뒤늦게 복잡해진다.

## 22. 성공 기준

v1 성공 기준은 다음과 같다.

- 하나의 Linear project를 대상으로 안정적으로 polling 가능하다
- 여러 issue를 독립 workspace에서 동시에 처리할 수 있다
- 실패 시 retry가 동작한다
- active 상태 변경 시 실행을 중단할 수 있다
- `WORKFLOW.md` 변경이 reload되고 invalid reload는 last-known-good로 버틴다
- 운영자가 로그와 상태 API만으로 현재 상태를 추적할 수 있다
- 문서만 보고 다른 저장소에도 복제 가능한 수준의 구조를 가진다

## 23. 다음 산출물

이 문서 다음 단계로 이어질 산출물은 다음과 같다.

1. `WORKFLOW.md` 초안
2. `AGENTS.md` 초안
3. `docs/architecture.md`
4. `docs/workflow-contract.md`
5. `cmd/harnessd/main.go`
6. `internal/domain` 타입 정의

## 24. 결정 메모

현재 시점의 권장 결정은 다음과 같다.

- 이름은 우선 `go-harness`로 둔다
- v1은 local worker only다
- v1은 Linear only다
- v1은 DB 없이 시작한다
- v1은 HTTP status surface를 권장 기본으로 제공한다
- CLI `status`는 같은 snapshotter를 재사용하는 thin wrapper다
- orchestrator는 상태 관리자이고, workflow 비즈니스 로직은 `WORKFLOW.md`와 agent에 위임한다
- tracker read는 orchestrator 내부 adapter가 담당한다
- tracker write는 injected high-level tool이 담당한다
- workflow template은 strict하고 Symphony 문법과 최대한 호환되게 설계한다
- live session, recent events, token totals, rate limits를 1급 observability state로 취급한다

이 결정이 유지되면 구현 난이도와 운영 복잡도를 동시에 낮출 수 있다.
