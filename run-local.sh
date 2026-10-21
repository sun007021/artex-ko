#!/usr/bin/env bash
# ARTEX 로컬 실행 도우미: Postgres(Docker) 켜기 → (필요하면) 빌드 → 서버 시작을 한 번에 합니다.
#
# 사용법:
#   ./run-local.sh              Postgres 를 켜고, (바뀐 부분만) 빌드한 뒤 서버를 포그라운드로 시작합니다(Ctrl-C 로 중지).
#   ./run-local.sh -d           같은 일을 하되 서버를 백그라운드로 띄웁니다. 터미널을 닫아도 계속 실행됩니다.
#   ./run-local.sh --no-build   빌드를 건너뜁니다(코드가 바뀌지 않았을 때).
#   ./run-local.sh --build      바뀐 게 없어도 화면과 서버를 전부 다시 빌드합니다.
#   ./run-local.sh status       백그라운드 서버가 떠 있는지 보여 줍니다.
#   ./run-local.sh logs         백그라운드 서버 로그를 따라 봅니다(Ctrl-C 로 빠져나옴, 서버는 유지).
#   ./run-local.sh stop         서버(백그라운드 포함)를 멈추고 Postgres 컨테이너를 내립니다(DB 데이터 볼륨은 남습니다).
#
# 빌드는 소스가 마지막 빌드보다 새로울 때만 합니다. 화면(web/)이 그대로면 화면 빌드를 건너뛰고,
# Go 소스와 화면 결과물이 ./artex 보다 오래됐으면 컴파일도 건너뜁니다. 상태 파일은 .run-local/ 에 있습니다.
#
# 준비물: Docker, Go, Node(첫 빌드의 화면 빌드용), 저장소 루트의 .env (POSTGRES_PASSWORD 필수).
# 접속 주소는 http://127.0.0.1:8787 입니다. Claude 구독(claude-code) 방식은 서버가 127.0.0.1 에
# 묶여 있을 때만 켜지므로 주소를 바꾸지 마십시오.
#
# 이미 ARTEX_PG_DSN 환경변수가 있으면 그 값을 그대로 쓰고, 없으면 .env 의 값으로 만듭니다.
# 비밀번호는 이 스크립트가 어디에도 저장하지 않습니다(.env 는 Git 에 올리지 마십시오).
set -euo pipefail
cd "$(dirname "$0")"

info() { printf '\033[36m[*]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

ADDR=127.0.0.1:8787
PROXY_ADDR=127.0.0.1:8788

STATE_DIR=.run-local
PID_FILE=$STATE_DIR/artex.pid
LOG_FILE=$STATE_DIR/artex.log
UI_STAMP=$STATE_DIR/ui-built

# docker compose 파일은 .env 에서 값을 읽는다. 여기서는 DSN 을 만들기 위해 필요한 세 값만 가져온다.
env_value() { # env_value KEY DEFAULT
  local line
  line=$(grep -E "^[[:space:]]*$1=" .env 2>/dev/null | tail -n 1 || true)
  line=${line#*=}
  line=${line%\"}; line=${line#\"}; line=${line%\'}; line=${line#\'}
  printf '%s' "${line:-$2}"
}

urlencode() {
  local s=$1 out="" c i
  for ((i = 0; i < ${#s}; i++)); do
    c=${s:i:1}
    case $c in
      [a-zA-Z0-9._~-]) out+=$c ;;
      *) out+=$(printf '%%%02X' "'$c") ;;
    esac
  done
  printf '%s' "$out"
}

ensure_docker() {
  command -v docker >/dev/null 2>&1 || die "docker 를 찾을 수 없습니다. Docker Desktop 을 설치하세요."
  if docker info >/dev/null 2>&1; then return; fi
  if [ "$(uname -s)" = Darwin ]; then
    info "Docker 가 꺼져 있어 Docker Desktop 을 엽니다..."
    open -a Docker || true
    for _ in $(seq 1 60); do
      docker info >/dev/null 2>&1 && return
      sleep 2
    done
  fi
  die "Docker 데몬에 연결할 수 없습니다. Docker 를 먼저 켜 주세요."
}

# 백그라운드로 띄운 start.sh 의 PID 를 돌려준다(떠 있을 때만 성공).
detached_pid() {
  local pid
  [ -f "$PID_FILE" ] || return 1
  pid=$(cat "$PID_FILE" 2>/dev/null || true)
  case $pid in '' | *[!0-9]*) return 1 ;; esac
  kill -0 "$pid" 2>/dev/null || return 1
  # PID 가 재사용됐을 수 있으니 start.sh 가 맞는지 확인한다.
  ps -p "$pid" -o command= 2>/dev/null | grep -q 'start\.sh' || return 1
  printf '%s' "$pid"
}

stop_detached() {
  local pid
  pid=$(detached_pid) || { rm -f "$PID_FILE"; return 0; }
  info "백그라운드 서버 중지(PID $pid)..."
  # start.sh 가 TERM 을 artex 로 넘겨 우아하게 종료시킨다.
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 30); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    info "30초 안에 끝나지 않아 강제 종료합니다."
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
}

stop_all() {
  stop_detached
  info "서버 중지..."
  pkill -f "start.sh -addr $ADDR" 2>/dev/null || true
  pkill -x artex 2>/dev/null || true
  ensure_docker
  info "Postgres 컨테이너 내리기(데이터 볼륨은 유지됩니다)..."
  docker compose down
  exit 0
}

show_status() {
  local pid
  if pid=$(detached_pid); then
    info "백그라운드 서버 실행 중 (PID $pid): http://$ADDR  로그: $LOG_FILE"
  else
    info "백그라운드 서버는 실행 중이 아닙니다."
  fi
  exit 0
}

show_logs() {
  [ -f "$LOG_FILE" ] || die "로그 파일이 없습니다: $LOG_FILE (./run-local.sh -d 로 먼저 실행하세요)"
  exec tail -n 100 -f "$LOG_FILE"
}

# REF 보다 새로운 파일이 PATHS 안에 하나라도 있으면 성공. REF 가 없으면 바뀐 것으로 본다.
changed_since() { # changed_since REF PATH...
  local ref=$1; shift
  [ -e "$ref" ] || return 0
  [ -n "$(find "$@" -type f -newer "$ref" -print -quit 2>/dev/null)" ]
}

ui_stale() {
  [ -d server/webui/dist ] || return 0
  changed_since "$UI_STAMP" web/src web/public web/messages web/package.json web/package-lock.json \
    web/next.config.mjs web/tsconfig.json web/postcss.config.mjs
}

go_stale() {
  [ -x ./artex ] || return 0
  [ -n "$(find . \( -path ./web -o -path ./.git \) -prune -o -type f \
    \( -name '*.go' -o -name go.mod -o -name go.sum \) -newer ./artex -print -quit 2>/dev/null)" ] && return 0
  changed_since ./artex server/webui/dist
}

DO_BUILD=1   # 1: 바뀐 부분만 빌드, 0: 건너뜀, 2: 전부 다시 빌드
DETACH=0
for arg in "$@"; do
  case $arg in
    stop) stop_all ;;
    status) show_status ;;
    logs) show_logs ;;
    -d | --detach) DETACH=1 ;;
    --no-build) DO_BUILD=0 ;;
    --build) DO_BUILD=2 ;;
    -h | --help) awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; exit 0 ;;
    *) die "알 수 없는 인자: $arg (사용법: ./run-local.sh [-d] [--no-build|--build] | stop | status | logs)" ;;
  esac
done

if pid=$(detached_pid); then
  die "백그라운드 서버가 이미 실행 중입니다(PID $pid, http://$ADDR). 멈추려면 ./run-local.sh stop, 다시 시작하려면 stop 후 다시 실행하세요."
fi

[ -f .env ] || die ".env 파일이 없습니다. POSTGRES_PASSWORD=... 를 담아 만드세요(.env.example 참고)."
[ -n "$(env_value POSTGRES_PASSWORD '')" ] || die ".env 에 POSTGRES_PASSWORD 가 없습니다."
if [ ! -f docker-compose.override.yml ]; then
  info "docker-compose.override.yml 이 없습니다. 호스트에서 Postgres(5432)에 닿으려면 포트를 열어야 합니다."
fi

ensure_docker
info "Postgres 시작..."
docker compose up -d --wait postgres

if [ -z "${ARTEX_PG_DSN:-}" ]; then
  user=$(env_value POSTGRES_USER artex)
  pass=$(env_value POSTGRES_PASSWORD '')
  db=$(env_value POSTGRES_DB artex)
  ARTEX_PG_DSN="postgres://$(urlencode "$user"):$(urlencode "$pass")@127.0.0.1:5432/${db}?sslmode=disable"
fi
export ARTEX_PG_DSN

if [ "$DO_BUILD" != 0 ]; then
  mkdir -p "$STATE_DIR"
  build_ui=0; build_go=0
  if [ "$DO_BUILD" = 2 ] || ui_stale; then build_ui=1; fi
  if [ "$build_ui" = 1 ] || [ "$DO_BUILD" = 2 ] || go_stale; then build_go=1; fi

  if [ "$build_ui" = 1 ]; then
    info "화면 + 서버 빌드(화면 빌드 때문에 오래 걸립니다)..."
    # 의존성이 lock 파일보다 새로 설치돼 있으면 npm ci 를 건너뛴다(느린 재설치 방지).
    if [ web/node_modules/.package-lock.json -nt web/package-lock.json ]; then export ARTEX_SKIP_NPM_CI=1; fi
    # 빌드 도중 수정한 파일을 놓치지 않도록 스탬프는 빌드 시작 시각으로 만든다.
    touch "$UI_STAMP.new"
    ARTEX_OUTPUT=./artex ./build.sh
    mv "$UI_STAMP.new" "$UI_STAMP"
  elif [ "$build_go" = 1 ]; then
    info "화면은 그대로라 건너뛰고 서버만 빌드합니다..."
    ARTEX_SKIP_FRONTEND=1 ARTEX_OUTPUT=./artex ./build.sh
  else
    info "바뀐 소스가 없어 빌드를 건너뜁니다(강제로 하려면 --build)."
  fi
fi
[ -x ./artex ] || die "./artex 가 없습니다. --no-build 없이 다시 실행하세요."

if [ "$DETACH" = 1 ]; then
  mkdir -p "$STATE_DIR"
  # 로그가 무한정 커지지 않게 10MB 를 넘으면 한 번 넘겨 둔다.
  if [ -f "$LOG_FILE" ] && [ "$(wc -c < "$LOG_FILE")" -gt 10485760 ]; then mv -f "$LOG_FILE" "$LOG_FILE.1"; fi
  info "서버를 백그라운드로 시작합니다..."
  # nohup 이 SIGHUP 을 무시하게 해서 터미널을 닫아도 살아남는다. 재시작·원클릭 업데이트는 start.sh 가 그대로 처리한다.
  nohup ./start.sh -addr "$ADDR" -proxy "$PROXY_ADDR" >>"$LOG_FILE" 2>&1 </dev/null &
  echo $! >"$PID_FILE"
  for _ in $(seq 1 30); do
    if ! detached_pid >/dev/null; then
      tail -n 20 "$LOG_FILE" >&2 || true
      rm -f "$PID_FILE"
      die "서버가 바로 종료됐습니다. 위 로그를 확인하세요($LOG_FILE)."
    fi
    if command -v curl >/dev/null 2>&1 && curl -s -o /dev/null --max-time 2 "http://$ADDR/"; then break; fi
    sleep 1
  done
  info "실행 중: http://$ADDR  (PID $(cat "$PID_FILE"), 로그: ./run-local.sh logs, 중지: ./run-local.sh stop)"
  exit 0
fi

info "서버 시작: http://$ADDR  (Ctrl-C 로 중지, 완전히 내리려면 ./run-local.sh stop)"
exec ./start.sh -addr "$ADDR" -proxy "$PROXY_ADDR"
