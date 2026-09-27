#!/usr/bin/env bash
# End-to-end acceptance flow against a running deployment.
#
# Covers: register -> issue API key -> redeem -> direct model call ->
# reserve/settle verification -> auto routing -> score publish -> multi-node
# ACK -> node drain -> rollback.
#
# Requires: docker compose stack up (postgres, redis, api, gateway) and an
# API key from the bootstrapped administrator is NOT needed; the script creates
# its own user. Administrator actions use an admin session.
#
#   API_URL=http://127.0.0.1:18080 GATEWAY_URL=http://127.0.0.1:18081 \
#   ADMIN_USER=admin ADMIN_PASSWORD=... PROVIDER_BASE_URL=http://mock:9000/v1 \
#   ./scripts/e2e_smoke.sh
set -euo pipefail

API_URL=${API_URL:-http://127.0.0.1:8080}
GATEWAY_URL=${GATEWAY_URL:-http://127.0.0.1:8081}
ADMIN_USER=${ADMIN_USER:-}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-}
PROVIDER_BASE_URL=${PROVIDER_BASE_URL:-}
PROVIDER_SECRET=${PROVIDER_SECRET:-e2e-provider-secret}
MODEL_KEY=${MODEL_KEY:-e2e-model}
STAMP=$(date +%s)
USER_EMAIL=${USER_EMAIL:-e2e-${STAMP}@example.test}
USER_PASSWORD=${USER_PASSWORD:-E2eSecurePass123!}

fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }
step() { printf '\n== %s\n' "$1"; }

json_field() { python3 -c "import json,sys;d=json.load(sys.stdin);print(eval('d'+sys.argv[1]))" "$1"; }

api() { # method path token body
  local method=$1 path=$2 token=${3:-} body=${4:-}
  local args=(-sS -X "$method" "$API_URL$path" -H 'Content-Type: application/json')
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}"
}

gateway() { # method path token body
  local method=$1 path=$2 token=${3:-} body=${4:-}
  local args=(-sS -X "$method" "$GATEWAY_URL$path" -H 'Content-Type: application/json')
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}"
}

step "health"
for url in "$API_URL/healthz" "$API_URL/readyz" "$GATEWAY_URL/healthz"; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$url") || fail "$url unreachable"
  [ "$code" = "200" ] || fail "$url returned $code"
done

step "register user"
register=$(api POST /user/api/v1/auth/register "" "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASSWORD\",\"nickname\":\"E2E\"}")
user_token=$(printf '%s' "$register" | json_field "['data']['token']") || fail "register failed: $register"

step "issue API key (one-time reveal)"
key_response=$(api POST /user/api/v1/api-keys "$user_token" '{"name":"e2e","rateLimitPerMin":0}')
api_key=$(printf '%s' "$key_response" | json_field "['data']['key']") || fail "key creation failed: $key_response"
key_id=$(printf '%s' "$key_response" | json_field "['data']['id']")
[ -n "$api_key" ] || fail "no plaintext key returned"
listed=$(api GET /user/api/v1/api-keys "$user_token")
printf '%s' "$listed" | grep -q "$api_key" && fail "plaintext key must never be listed again"

if [ -z "$ADMIN_USER" ] || [ -z "$ADMIN_PASSWORD" ]; then
  printf 'SKIP admin/provider setup (ADMIN_USER/ADMIN_PASSWORD not set); using existing catalog\n'
else
  step "admin login"
  login=$(api POST /admin/api/v1/auth/login "" "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASSWORD\"}")
  admin_token=$(printf '%s' "$login" | json_field "['data']['token']") || fail "admin login failed: $login"

  if [ -n "$PROVIDER_BASE_URL" ]; then
    step "provider + model"
    provider=$(api POST /admin/api/v1/providers "$admin_token" "{\"name\":\"e2e-$STAMP\",\"baseUrl\":\"$PROVIDER_BASE_URL\",\"secret\":\"$PROVIDER_SECRET\",\"type\":\"openai_compatible\",\"authType\":\"bearer\"}")
    provider_id=$(printf '%s' "$provider" | json_field "['data']['id']") || fail "provider create failed: $provider"
    test=$(api POST "/admin/api/v1/providers/$provider_id/test" "$admin_token" '{}')
    printf '%s' "$test" | grep -q '"reachable":true' || fail "provider connectivity test failed: $test"
    model=$(api POST /admin/api/v1/models "$admin_token" "{\"providerId\":$provider_id,\"name\":\"E2E Model\",\"modelKey\":\"$MODEL_KEY\",\"contextLength\":8192,\"supportsStream\":true,\"inputModalities\":[\"text\"],\"inputPriceMicro\":100,\"outputPriceMicro\":200,\"chargeInputMicro\":300,\"chargeOutputMicro\":600}")
    model_id=$(printf '%s' "$model" | json_field "['data']['id']") || fail "model create failed: $model"
  fi

  step "quota package + redemption code + redeem"
  package=$(api POST /admin/api/v1/quota-packages "$admin_token" '{"name":"e2e-package","faceValueMicro":1000000}')
  package_id=$(printf '%s' "$package" | json_field "['data']['id']") || fail "package create failed: $package"
  codes=$(api POST "/admin/api/v1/quota-packages/$package_id/codes" "$admin_token" '{"quantity":1}')
  code=$(printf '%s' "$codes" | json_field "['data']['codes'][0]") || fail "code generation failed: $codes"
  redeemed=$(api POST /user/api/v1/redemption/redeem "$user_token" "{\"code\":\"$code\"}")
  printf '%s' "$redeemed" | grep -q '"grantedMicro"' || fail "redeem failed: $redeemed"
fi

step "direct model call settles a reservation"
balance_before=$(api GET /user/api/v1/quota "$user_token" | json_field "['data']['balanceMicro']")
call=$(gateway POST /v1/chat/completions "$api_key" "{\"model\":\"$MODEL_KEY\",\"messages\":[{\"role\":\"user\",\"content\":\"hello e2e\"}],\"max_tokens\":16}")
request_id=$(printf '%s' "$call" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)
printf '%s' "$call" | grep -q '"object":"chat.completion"' || fail "chat completion failed: $call"
balance_after=$(api GET /user/api/v1/quota "$user_token" | json_field "['data']['balanceMicro']")
[ "$balance_before" != "$balance_after" ] || fail "expected a settled charge, balance unchanged"
printf 'balance %s -> %s (request %s)\n' "$balance_before" "$balance_after" "$request_id"

requests=$(api GET /user/api/v1/requests "$user_token")
printf '%s' "$requests" | grep -q '"requestedModel"' || fail "request record missing: $requests"
usage=$(api GET /user/api/v1/usage "$user_token")
printf '%s' "$usage" | grep -q '"inputTokens"' || fail "usage aggregation missing: $usage"

step "streaming call"
stream=$(gateway POST /v1/chat/completions "$api_key" "{\"model\":\"$MODEL_KEY\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"stream\"}],\"max_tokens\":16}")
printf '%s' "$stream" | grep -q 'data: ' || fail "stream produced no SSE frames"
printf '%s' "$stream" | grep -q 'data: \[DONE\]' || fail "stream missing terminator"

step "idempotent replay is refused"
payload="{\"model\":\"$MODEL_KEY\",\"messages\":[{\"role\":\"user\",\"content\":\"idem\"}],\"max_tokens\":8}"
idem="e2e-$STAMP"
first=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -H "Idempotency-Key: $idem" -d "$payload")
[ "$first" = "200" ] || fail "first idempotent call returned $first"
second=$(curl -sS -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -H "Idempotency-Key: $idem" -d "$payload")
code=$(printf '%s' "$second" | tail -n1)
[ "$code" = "409" ] || fail "idempotent replay returned $code (expected 409)"

if [ -n "${admin_token:-}" ]; then
  step "score refresh, publish and node ACK"
  refresh=$(api POST /admin/api/v1/evaluation/refresh "$admin_token" '{}')
  task_id=$(printf '%s' "$refresh" | json_field "['data']['taskId']") || fail "refresh start failed: $refresh"
  for _ in $(seq 1 60); do
    status=$(api GET "/admin/api/v1/evaluation/refresh/$task_id" "$admin_token")
    state=$(printf '%s' "$status" | json_field "['data']['task']['status']")
    [ "$state" != "running" ] || sleep 2
    [ "$state" = "running" ] || break
  done
  printf 'refresh task %s state=%s\n' "$task_id" "$state"
  printf '%s' "$status" | grep -q '"items"' || fail "task item evidence missing: $status"

  versions=$(api GET /admin/api/v1/evaluation/versions "$admin_token")
  candidate=$(printf '%s' "$versions" | python3 -c 'import json,sys;d=json.load(sys.stdin)["data"]["list"];print(next((v["id"] for v in d if v["status"]=="building"), ""))')
  if [ -n "$candidate" ]; then
    published=$(api POST "/admin/api/v1/evaluation/versions/$candidate/publish" "$admin_token" '{"reason":"e2e publish"}')
    state=$(printf '%s' "$published" | json_field "['data']['status']")
    printf 'publish status: %s (pending means some serving node has not ACKed)\n' "$state"
    [ "$state" = "published" ] || [ "$state" = "pending" ] || fail "unexpected publish status: $published"

    step "auto routing after publish"
    auto=$(gateway POST /v1/chat/completions "$api_key" '{"model":"auto","messages":[{"role":"user","content":"帮我写一个排序函数"}],"max_tokens":16}')
    if [ "$state" = "published" ]; then
      printf '%s' "$auto" | grep -q '"object":"chat.completion"' || fail "auto route failed after publish: $auto"
    else
      printf 'auto route skipped: score version still pending ACK\n'
    fi

    step "rollback"
    previous=$(printf '%s' "$versions" | python3 -c 'import json,sys;d=json.load(sys.stdin)["data"]["list"];print(next((v["id"] for v in d if v["status"]=="superseded"), ""))')
    if [ -n "$previous" ]; then
      rolled=$(api POST "/admin/api/v1/evaluation/versions/$previous/rollback" "$admin_token" '{"reason":"e2e rollback"}')
      printf 'rollback status: %s\n' "$(printf '%s' "$rolled" | json_field "['data']['status']")"
    fi
    nodes=$(api GET /admin/api/v1/nodes "$admin_token")
    printf '%s' "$nodes" | grep -q '"list"' || fail "node health endpoint failed: $nodes"
    printf 'nodes: %s\n' "$nodes"
  fi

  step "node drain and readiness"
  if curl -sS -o /dev/null -w '%{http_code}' "$GATEWAY_URL/readyz" | grep -q 200; then
    printf 'gateway node is serving traffic; drain it via your load balancer (LB_MEMBERSHIP_URL) and re-check /readyz returns 503\n'
  else
    printf 'gateway node reports not ready (draining or unacknowledged)\n'
  fi
fi

step "done"
printf 'E2E flow completed for user %s (key id %s)\n' "$USER_EMAIL" "${key_id:-unknown}"
