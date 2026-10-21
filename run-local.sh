#!/usr/bin/env bash
# ARTEX 로컬 실행 도우미: Postgres(Docker) 켜기 → (필요하면) 빌드 → 서버 시작을 한 번에 합니다.
#
# 사용법:
#   ./run-local.sh              Postgres 를 켜고, 빌드한 뒤 서버를 포그라운드로 시작합니다(Ctrl-C 로 중지).
#   ./run-local.sh --no-build   빌드를 건너뜁니다(코드가 바뀌지 않았을 때).
#   ./run-local.sh stop         서버를 멈추고 Postgres 컨테이너를 내립니다(DB 데이터 볼륨은 남습니다).
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

stop_all() {
  info "서버 중지..."
  pkill -f "start.sh -addr $ADDR" 2>/dev/null || true
  pkill -x artex 2>/dev/null || true
  ensure_docker
  info "Postgres 컨테이너 내리기(데이터 볼륨은 유지됩니다)..."
  docker compose down
  exit 0
}

DO_BUILD=1
for arg in "$@"; do
  case $arg in
    stop) stop_all ;;
    --no-build) DO_BUILD=0 ;;
    -h | --help) sed -n '2,12p' "$0"; exit 0 ;;
    *) die "알 수 없는 인자: $arg (사용법: ./run-local.sh [--no-build|stop])" ;;
  esac
done

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

if [ "$DO_BUILD" = 1 ]; then
  info "빌드(첫 실행은 화면 빌드 때문에 오래 걸립니다)..."
  # 의존성이 lock 파일보다 새로 설치돼 있으면 npm ci 를 건너뛴다(느린 재설치 방지).
  if [ web/node_modules/.package-lock.json -nt web/package-lock.json ]; then export ARTEX_SKIP_NPM_CI=1; fi
  ARTEX_OUTPUT=./artex ./build.sh
fi
[ -x ./artex ] || die "./artex 가 없습니다. --no-build 없이 다시 실행하세요."

info "서버 시작: http://$ADDR  (Ctrl-C 로 중지, 완전히 내리려면 ./run-local.sh stop)"
exec ./start.sh -addr "$ADDR" -proxy "$PROXY_ADDR"
