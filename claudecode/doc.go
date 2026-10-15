// Package claudecode lets ARTEX run its LLM turns on the owner's own Claude Pro/Max
// subscription by driving the official, unmodified `claude` CLI (Claude Code) in
// headless mode — on the owner's own machine only.
//
// 정책 경계(바꾸지 말 것). 출처: https://code.claude.com/docs/en/legal-and-compliance ,
// https://code.claude.com/docs/en/authentication , https://www.anthropic.com/legal/consumer-terms
//
//   - 인증은 전적으로 claude 바이너리 안에서 끝난다. 이 패키지는 ~/.claude/.credentials.json,
//     키체인, CLAUDE_CODE_OAUTH_TOKEN 을 읽지 않고, api.anthropic.com 을 직접 부르지 않으며,
//     Claude Code 를 흉내 내는 헤더를 만들지 않는다. 로그인 상태는 `claude auth status`
//     (JSON)의 loggedIn/authMethod 만 읽는다(status.go).
//   - 바이너리를 수정·래핑하지 않는다. 문서화된 CLI 플래그만 쓴다(args.go). --bare 는 구독
//     로그인을 무시하므로 쓰지 않는다.
//   - 자식 프로세스 환경은 허용 목록(PATH, HOME, 로캘, 프록시·TLS, CLAUDE_CONFIG_DIR ...)으로
//     만든다(env.go). ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN / CLAUDE_CODE_OAUTH_TOKEN /
//     ANTHROPIC_BASE_URL / CLAUDE_CODE_USE_* 등 목록에 없는 변수는 모두 빠진다. 자식은 사용자가
//     직접 로그인한 구독을 쓴다.
//   - 동시에 모델 응답을 만드는 claude 프로세스 수는 전역으로 제한한다(limiter.go, 기본 1,
//     하드 상한 3). 사용량 한도/속도 제한이면 계정을 바꾸지 않고 물러난다(limiter.go).
//   - MCP 엔드포인트는 127.0.0.1 에만 바인드한다(mcp.go).
//
// 구조(공급자 브리지): ARTEX 의 agentcore 루프가 계속 주도한다. Provider.Stream 한 번이
// 모델 한 턴이며, 세션(ARTEX 세션 ID 당 장수명 `claude -p --input-format stream-json`
// 프로세스)이 그 턴을 만든다. ARTEX 도구 스키마는 loopback MCP 서버로 노출되지만 그 핸들러는
// 아무것도 실행하지 않는다 — tool_use 를 agentcore 에 돌려주고(Stream 이 반환), agentcore 가
// 권한·훅·캡처·전사와 함께 도구를 실행한 뒤 다음 Stream 호출에서 넘긴 결과를 보류 중이던 MCP
// 호출의 응답으로 돌려준다.
//
// 코드 구성:
//
//	manager.go   Manager: 한 설치의 실행 환경(세션 표, 동시 실행 슬롯, 사용량 한도 게이트, MCP 허브, 점검 캐시).
//	             서버가 하나 만들어 프로필 설정에 주입한다(전역 상태 없음).
//	provider.go  Provider.Stream: 요청 → 세션 계획(plan) → 한 번의 시도(attempt) → 재동기화 재시도 판단.
//	session.go   세션 상태 기계(idle/busy/parked)와 대화 서명, 전사 재생(resync).
//	turn.go      한 턴의 소비(consume): 이벤트 중계, 종료 유실 감지, 무활동 감시, 취소.
//	process.go   proc: claude 프로세스 하나의 수명(spawn/kill/종료 감시)과 무활동 시계.
//	callpair.go  callPairer: MCP tools/call 을 agentcore 가 돌려줄 tool_result 와 짝짓는다(_meta 의 tool_use ID 우선).
//	translator.go streamTranslator: claude stdout 의 stream-json 줄 → llm 스트림 이벤트.
//	toolbox.go   노출 도구 목록과 지문(도구 집합이 바뀌면 재동기화).
//	limiter.go   동시 실행 슬롯, 사용량 한도 게이트(모델별/전역).
//	prober.go    `claude --version`/`auth status` 점검의 singleflight 캐시.
package claudecode
