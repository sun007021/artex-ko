# 변경 이력

한국어 · [English](CHANGELOG.en.md) · [中文(원본·상류)](CHANGELOG.zh.md)

이 문서는 ARTEX 한국어판(이 포크)이 상류 저장소에 더한 변경을 기록합니다. 형식은 [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) 를 참고합니다.

상류 ARTEX 프로젝트의 버전별 릴리스 이력(0.3.x 이하)과 기여자 목록은 원본 중국어 그대로 [`CHANGELOG.zh.md`](CHANGELOG.zh.md) 에 보존했습니다. 상류 변경과 대조하기 쉽도록 `README.zh.md` 와 같은 방식으로 원문을 그대로 남깁니다. 각 변경의 자세한 내용과 근거는 저장소 커밋 이력에서 확인할 수 있습니다.

## [Unreleased] · 한국어판 변경

### LLM 연결

- **ChatGPT Plus/Pro 구독으로 LLM 을 돌릴 수 있습니다(API Key 불필요).** LLM 프로필에 인증 방식 `chatgpt` 를 추가했습니다. OpenAI 가 오픈소스·로컬 앱에 공식으로 연 「Sign in with ChatGPT」 토큰 공유 경로(OIDC + PKCE, `127.0.0.1` 콜백)를 쓰고, 추론은 공개 Responses API 에 Bearer 토큰으로 보냅니다(`store:false`, 스트리밍). 에이전트 루프·도구 호출은 기존 `openai-responses` 경로를 그대로 씁니다. 설정 화면에 「ChatGPT로 로그인」 버튼, 계정 모델 드롭다운, 추론 강도 선택을 더했고, 서버가 다른 호스트일 때를 위한 주소 붙여넣기 완료 경로를 두었습니다. 토큰은 서버 DB 에만 저장되며 화면·API 응답·로그·LLM 기록에는 나오지 않습니다. 한도 초과(`subscription_sharing_usage_limit_exceeded`)는 유료 API Key 설정으로 자동 장애 전환하지 않고 오류로 드러냅니다(OpenAI 문서: 구독 요청을 다른 결제 경로로 자동 전환 금지). 사용법과 제한(출력 토큰 상한 미지원, 평문 DB 보관, 개인·로컬 사용 범위)과 정책 근거는 [README](README.md#chatgpt-구독으로-사용하기-api-key-없이)에 있습니다. 서드파티 앱이 Claude 구독 자격 증명을 대신 쓰는 방식은 지원하지 않으며, Claude 구독은 아래의 별도 방식으로만 씁니다.
- **ChatGPT 구독 경로를 문서 규칙과 안정성 기준에 맞춰 보강했습니다.** 도구를 쓰는 턴이 거부되지 않도록 function 도구를 namespace 로 묶어 보내고 응답의 도구 이름을 ARTEX 이름으로 되돌립니다. 토큰 갱신·저장은 요청 취소와 무관하게 끝까지 진행되고(회전된 refresh token 유실 방지), 프로필당 세션은 하나이며 프록시가 바뀌어도 유지됩니다. 갱신 실패에는 백오프를 두고, 토큰 외 원인의 401 에 갱신을 되풀이하지 않으며, 앱 등록 거부(`invalid_client`)는 상태 화면에 이유와 함께 보입니다. 다른 계정으로 로그인되면 새 토큰을 폐기하고, 잘못된 프록시는 직접 연결로 우회하지 않으며 로그에 자격 증명을 남기지 않습니다. `/auth/start` 주소는 한 번만 쓸 수 있고, 모델 없는 구독 프로필은 저장되지 않습니다.
- **ChatGPT 구독 프로필에 Daybreak 켜기/끄기를 더했습니다.** OpenAI Daybreak(Trusted Access for Cyber) 승인을 받은 계정이 쓸 수 있도록, 프로필의 「Daybreak 켜기」 스위치가 켜져 있으면 요청마다 `access_programs.cyber=daybreak_blue` 를 보냅니다. 기본은 꺼짐이며 꺼져 있으면 요청 본문을 전혀 바꾸지 않아 기존 동작과 같습니다. Blue 만 지원합니다. 같은 스위치가 `openai-responses` 형식의 API Key 프로필에도 있고(요청 본문에 같은 필드를 더함), 그 밖의 프로필에서는 서버가 값을 끕니다. Claude 구독(`claude-code`) 프로필의 모델 칸에는 Fable 5.1·Mythos 5.1·Opus/Sonnet/Haiku 5.5 등 내장 모델 목록을 고르는 버튼을 더했습니다(직접 입력은 그대로). `claude` CLI 에 모델 조회 명령이 없고 구독 토큰으로 API 를 부를 수 없어 실시간 조회가 아닌 내장 목록입니다.

- **Claude Pro/Max 구독을 개인 로컬 사용 한정으로 쓸 수 있습니다(기본 꺼짐, 옵트인).** LLM 프로필에 인증 방식 `claude-code` 를 추가했습니다. 사용자가 직접 설치·로그인한 공식 `claude` CLI 를 헤드리스 모드(`claude -p`, stream-json)로 구동하는 공급자 브리지로, 에이전트 루프·권한·승인·훅·트래픽 캡처·발견/자산/그래프 기록·전사는 API Key 경로와 같은 ARTEX 코드가 처리하고 모델 한 턴만 `claude` 가 만듭니다. ARTEX 는 Claude 자격 증명을 읽거나 저장하거나 전달하지 않고(`claude auth status` 의 로그인 여부와 방식만 읽음) `claude` 바이너리를 수정하지 않으며 `--bare` 도 쓰지 않습니다. 자식 프로세스 환경에서 `ANTHROPIC_API_KEY`·`ANTHROPIC_AUTH_TOKEN` 등 인증·라우팅 변수를 제거하고, `authMethod` 가 구독(`claude.ai`/`oauth_token`)이 아니면 API 과금 경로로 보고 거절합니다. ARTEX 도구는 `127.0.0.1` 전용 MCP(프로세스별 256비트 토큰, Host/Origin 검사)로만 노출됩니다. 동시에 모델 응답을 만드는 `claude` 프로세스는 전역 상한 1(최대 3)이고, 사용량 한도는 해제 시각까지 재요청 없이 멈추며 기존 장애 전환으로 이어집니다. 설정 화면에는 개인 로컬 사용 전용·타인 노출 금지·허가된 대상만·Claude 거절 우회 금지·Anthropic 제재 가능성을 명시한 고지와 켜기 확인란, 로그인 상태(`claude auth login` 은 사용자가 직접 실행), 모델·추론 강도(`--model`/`--effort`), 동시 실행 상한이 들어 있습니다. API Key 경로와의 차이(claude 자체 루프·압축 병행, 압축/재시작 뒤 텍스트 전사 재생, 출력 상한 등 미적용)와 정책의 불확실한 점은 [README](README.md#claude-구독으로-사용하기-공식-claude-cli-개인-로컬-사용-전용)에 있습니다.
- **Claude 구독 브리지를 안정성 기준에 맞춰 보강했습니다.** 자식 `claude` 의 환경변수를 거부 목록 대신 허용 목록으로 만들어(`CLAUDE_CODE_USE_MANTLE` 같은 새 경로 전환 변수도 자동 차단), 응답 중 출력이 10분간 없으면 멈춘 프로세스를 정리해 유일한 슬롯을 돌려줍니다(`api_retry` 중에는 횟수 상한까지 연장). 대화 도중 도구 목록이 늘거나 스키마가 바뀌면 새 프로세스로 재동기화합니다(`claude` 는 도구 목록을 한 번만 읽습니다). 프롬프트의 초 단위 `{{.Now}}` 는 해시에서 시 단위로만 보므로 매 턴 재시작하지 않습니다(프롬프트 문구는 그대로). 사용량 한도 차단은 5시간·주간 한도만 전역이고 모델별 한도는 그 모델 프로필에만 적용됩니다. 실행 중 턴에 기능을 끄거나 세션을 닫아도 고아 `claude` 프로세스가 남지 않고, 종료 신호가 유실돼도 슬롯을 쥐지 않으며, 살아 있는 프로세스 상한(`ARTEX_CLAUDE_CODE_MAX_LIVE`, 기본 8)을 넘으면 유휴 세션부터 정리하고 로그로 남깁니다. 같은 도구·같은 인자의 병렬 호출은 MCP `_meta` 의 `tool_use` ID 로 짝지으며, 이름만으로 짝지은 경우는 로그에 남깁니다. 내부적으로는 `Manager` 를 서버가 소유·주입하고(전역 `defaultManager` 제거) 세션 코드를 호출 짝맞춤·스트림 번역·프로세스 수명·턴 소비로 나눴으며, 구독 방식 프로필(ChatGPT·claude-code) 정규화는 `db` 한 곳에서 합니다.

### 보안

- **첫 실행 관리자 계정의 원격 선점을 막았습니다.** 비밀번호가 아직 없는 인스턴스는 `POST /api/auth/init` 을 인증 없이 받아, 포트에 먼저 닿은 누구나 관리자 비밀번호를 정하고 JWT 를 받을 수 있었습니다(비공개 제보: Dangel). 이제 기동 시 서버 콘솔에만 출력되는 설정 토큰(`setup_token`)이 맞아야 초기화되고, 틀린 시도는 횟수 제한에 걸립니다. 무인 배포용으로 `ARTEX_SETUP_TOKEN`·`ARTEX_ADMIN_PASSWORD` 환경 변수를 추가했고, `/setup` 화면에 토큰 입력란을 넣었습니다.
- **기본 바인딩을 127.0.0.1 로 바꿨습니다.** `artex -addr` 기본값이 `:8787` 에서 `127.0.0.1:8787` 로, `docker-compose.yml` 의 8787 게시가 `${ARTEX_BIND:-127.0.0.1}` 로 바뀝니다. 다른 기기에서 접속하던 배포는 `-addr :8787` 또는 `.env` 의 `ARTEX_BIND=0.0.0.0` 을 지정해야 합니다. 비밀번호 없이 루프백이 아닌 주소에 바인딩하면 기동 로그에 경고가 찍힙니다.
- **비밀번호를 바꾸면 기존 토큰이 모두 무효화됩니다.** JWT 서명 키를 비밀번호 해시에서 파생해, 화면 변경·`reset-password.sh`·DB 직접 수정 어느 경로든 이전 세션이 끊깁니다. 업그레이드 직후에도 한 번 다시 로그인해야 합니다. 비밀번호 변경 응답에 새 토큰이 실려 현재 브라우저 세션은 이어집니다.
- **CORS 전면 허용을 허용 목록으로 바꿨습니다.** `Access-Control-Allow-Origin: *` 대신 `ARTEX_CORS_ORIGINS` 에 적힌 출처(기본값은 `next dev` 의 `http://localhost:5173`)에만 헤더를 줍니다. 서버 측 최소 비밀번호 길이(8자) 검사와 초기화·로그인 실패 로그(클라이언트 주소 포함)도 함께 추가했습니다.
- **상류 ARTEX 0.3.15 의 인증 하드닝을 이식했습니다.** 비밀번호 관련 설정 읽기가 실패하면 이제 `initialized:false` 로 포장하지 않고 503 으로 닫습니다(`authStatus`·`authInit`·`authLogin`·비밀번호 변경). 최초 비밀번호 설정은 `INSERT ... ON CONFLICT DO NOTHING`([`db.InsertSettingIfAbsent`](db/settings.go))으로 원자화해, 읽기 검사가 흔들려도 기본키 제약이 기존 값을 지킵니다. 72바이트를 넘는 비밀번호는 500 대신 400 으로 거부하고, 데이터베이스 커넥션 풀 상한(32)을 두어 `too many clients` 오류가 fail-open 판단을 흔드는 경로를 줄였습니다. 상류 커밋 `a951e4a`(0.3.15)에 맞춰 이식했으며, 이 포크에는 PR #20 과 겹치지 않는 부분만 더했습니다.

### 현지화 (i18n)

- **영어·중국어·스페인어 전환을 추가했습니다.** 기본 언어는 그대로 한국어이고, 설정 화면의 "표시 언어"에서 고르면 웹 UI(`web/messages/{ko,en,zh,es}.json`)와 에이전트가 쓰는 리포트·요약·대화 응답, 서버 오류 메시지·상태 라벨이 함께 바뀝니다. 운영자 콘솔 로그와 서버 기본값은 `ARTEX_LANG`(또는 설정 파일의 `language`)으로 정합니다. 에이전트의 행동 지침 본문은 이번에도 원문 그대로 둡니다.
- **사용자 노출 출력을 한국어로 강제했습니다.** 벤치마크된 에이전트의 행동 지침 본문(두뇌)은 성능 보존을 위해 원문 그대로 두고, 코드 고정 세그먼트(`langDirective`)로 사용자에게 보이는 산출물(취약점 리포트, 사실 요약, 최종 요약, 채팅 응답)만 한국어로 작성하도록 지시합니다. 명령·페이로드·코드·로그 원문은 원본을 보존합니다.
- **웹 UI 를 한국어로 옮겼습니다.** Next App Router 에 `next-intl` 을 도입하고 문자열을 `web/messages/ko.json` 과 `web/messages/zh.json` 으로 분리했습니다. 원본 중국어는 `zh.json` 에 보존해 상류 업데이트와 대조합니다. 대시보드·취약점·대화·알림 발송·가로채기·LLM 설정 등 화면 문자열을 한국어로 옮겼습니다.
- **서버 API 의 사용자 노출 오류·응답을 한국어로 옮겼습니다.** 브라우저로 돌아가는 HTTP 오류·응답 문구를 한국어로 교체했습니다. 단 에이전트 두뇌의 입력으로 되먹여지는 문구는 벤치마크 드리프트를 막기 위해 원문을 유지했고, 그 판정 근거는 저장소 작업 문서에 기록했습니다.
- **문서를 한국어로 정비했습니다.** 한국어 `README.md` 를 만들고 영어 `README.en.md` 를 함께 두었으며, 원본 중국어는 `README.zh.md` 로 보존했습니다.

### 방어·탐지 자료

- **방어·탐지 가이드를 추가했습니다.** 자율 AI 공격이 기존 스캐너와 무엇이 다른가, 방어자가 관측할 수 있는 지문(IoC), 진입점과 하드닝, 탐지 규칙, 사고 대응을 정리한 한국어 가이드([`docs/defense-ko.md`](docs/defense-ko.md))와 같은 내용의 영어판([`docs/defense-en.md`](docs/defense-en.md))을 두었습니다.
- **배포용 탐지 규칙을 제공합니다.** 가이드의 지문을 바로 쓸 수 있는 규칙으로 옮겼습니다. 호스트·로그 계층은 [Sigma](https://sigmahq.io) 원자·상관 규칙([`detections/sigma/`](detections/sigma/)), 네트워크 계층은 enrich 프로브와 norma SDK WebFetch 의 User-Agent 를 겨냥한 [Suricata](https://suricata.io) 규칙([`detections/suricata/`](detections/suricata/))으로 담았습니다.
- **ATT&CK 커버리지를 가시화했습니다.** 규칙이 태깅하는 기법을 MITRE ATT&CK Navigator 레이어([`detections/attack/`](detections/attack/))로 정리했습니다.
- **기계 판독 침해지표(IoC)를 표준 형식으로 제공합니다.** ARTEX 가 내보내는 고유 지문을 한 파일로 모은 CSV([`detections/indicators/artex_indicators.csv`](detections/indicators/artex_indicators.csv))와, 같은 지표를 위협 인텔리전스 플랫폼에 바로 가져올 수 있는 MISP 이벤트([`detections/indicators/artex_indicators.misp.json`](detections/indicators/artex_indicators.misp.json))로 담았습니다. 규칙이 받쳐 주는 지표는 `to_ids` 로, 호스트 포렌식 포트는 분류용 단서로 구분해 표기합니다.
- **재현 가능한 탐지 테스트를 붙였습니다.** 규칙을 실제로 돌려 증명하는 테스트 여덟 종(Sigma 구조·컴파일 검증, Sigma 실시간 이벤트 매칭, 백엔드 이식성, SigmaHQ 관례 린트, Suricata 로드·발화, ATT&CK 레이어 정합, 지표-소스 일치, MISP 내보내기 ↔ CSV 동기화)과 이를 한 번에 돌리는 일괄 러너·pre-commit 예시를 추가하고 CI 머지 게이트로 연결했습니다. Sigma 실시간 이벤트 매칭은 규칙이 컴파일될 뿐 아니라 악성 샘플 이벤트에는 실제로 발화하고 정상 이벤트에는 침묵하는지까지 원자·상관 규칙 모두에서 확인합니다.

### 저장소 정비

- **보안·오남용 경고와 국내법 고지를 넣었습니다.** README 최상단에 사용 범위, 정보통신망법·개인정보보호법 고지, 오남용 금지 경고를 추가했습니다.
- **한국어 UI 스크린샷으로 화면 미리 보기를 교체했습니다.**
- **메인테이너 런북과 기여 가이드를 정비했습니다.** 상류 동기화·번역 드리프트를 막기 위한 런북([`MAINTAINING.md`](MAINTAINING.md))과 탐지 규칙 기여 계약([`CONTRIBUTING.md`](CONTRIBUTING.md))을 두었습니다. 런북에는 릴리스 발행 파이프라인의 빌드 전제와, 태그 없이 로컬에서 그 전제를 검증하는 절차도 함께 정리했습니다.
- **푸시·PR 머지 게이트 CI 를 추가했습니다.** 상류 저장소는 태그 릴리스에서만 CI 가 돌았지만, 이 포크는 모든 푸시와 PR 에서 Go 빌드·정적 분석(`go vet`)·단위 테스트([`ci.yml`](.github/workflows/ci.yml)), 한국어 UI 정적 빌드([`web.yml`](.github/workflows/web.yml)), 문서의 저장소 내부 링크·이미지 참조 무결성([`docs.yml`](.github/workflows/docs.yml))을 돌려, 한국어화 과정에서 생긴 회귀를 머지 전에 잡습니다. 데이터베이스가 있어야 하는 통합 테스트는 패키지마다 격리된 PostgreSQL 서비스로 함께 검증합니다. 문서 링크 검사는 외부 네트워크에 의존하지 않는 결정론적 스크립트([`scripts/check-doc-links.py`](scripts/check-doc-links.py))로 돌려, 다국어 문서가 서로를 가리키는 많은 상대 링크와 화면 미리 보기 이미지가 깨진 채 머지되는 것을 막습니다. 문서 앵커(`#헤딩`) 링크도 GitHub 과 같은 slug 규칙으로 헤딩과 대조해, 헤딩 글자가 바뀌어 조용히 끊긴 목차·상호 참조 링크를 함께 잡습니다. 탐지 규칙 스위트는 위 '방어·탐지 자료' 절에서 설명한 머지 게이트가 담당합니다.
- **외부 링크 생존을 주기적으로 점검합니다.** 방어 가이드가 가리키는 사고 신고 창구·표준 참조 같은 외부 링크는 원격 서버 상태에 의존해 flaky 하므로 머지 게이트에서 빼고, 비차단 워크플로([`external-links`](.github/workflows/external-links.yml))가 매주 월요일과 수동 실행으로 브라우저 User-Agent·GET·리다이렉트 추적 점검([`scripts/check-external-links.py`](scripts/check-external-links.py))을 돌립니다. 호스트는 살아 있는데 확인 방법만 막힌 경우(봇 차단·속도 제한)와 우리가 고칠 수 없는 상류 상속 죽은 링크(allowlist)는 실패로 치지 않아, 우리 문서가 큐레이션한 외부 링크가 새로 깨질 때만 빨갛게 드러냅니다.
- **기여·거버넌스 인프라를 갖췄습니다.** 버그·기능·번역 이슈 템플릿([`.github/ISSUE_TEMPLATE/`](.github/ISSUE_TEMPLATE/))과 풀 리퀘스트 템플릿([`PULL_REQUEST_TEMPLATE.md`](.github/PULL_REQUEST_TEMPLATE.md)), 보안 취약점 신고 정책([`SECURITY.md`](SECURITY.md)), 행동 강령([`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md))을 두어, 외부 기여자가 이슈·PR·보안 신고를 일관된 양식으로 제출하도록 했습니다.
- **해외 기여자를 위한 영어 문서 레이어를 완성했습니다.** 이 저장소는 한국어가 주 언어이지만, 한국어를 읽지 못하는 기여자·보안 연구자·방어자가 같은 정보에 도달하도록 핵심 문서의 영어판을 함께 두었습니다. 영어 `README.en.md`·방어 가이드([`docs/defense-en.md`](docs/defense-en.md))에 더해, 변경 이력([`CHANGELOG.en.md`](CHANGELOG.en.md)), 보안 신고 정책([`SECURITY.en.md`](SECURITY.en.md)), 행동 강령([`CODE_OF_CONDUCT.en.md`](CODE_OF_CONDUCT.en.md)), 기여 가이드([`CONTRIBUTING.en.md`](CONTRIBUTING.en.md)), 메인테이너 런북([`MAINTAINING.en.md`](MAINTAINING.en.md)), 트래픽 증거 설계 문서([`docs/finding-traffic-evidence-en.md`](docs/finding-traffic-evidence-en.md)), 그리고 버그·기능·번역 이슈 템플릿의 영어판을 갖췄습니다. 한국어판과 영어판은 머리말에서 서로를 가리켜, 어느 언어로 들어와도 반대쪽으로 이동할 수 있습니다. (풀 리퀘스트 템플릿은 현재 한국어판만 제공합니다.)

---

상류 ARTEX 프로젝트의 버전별 릴리스 이력과 기여자 목록은 [`CHANGELOG.zh.md`](CHANGELOG.zh.md) 에서 원문 그대로 볼 수 있습니다.
