"use client";

import * as React from "react";

import {
  ListIcon,
  Loader2Icon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SaveIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { ChatGPTStatus, LLMPoolMember, LLMPoolStatus, LLMProfile, LLMRetryOverride } from "@/lib/types";
import { cn } from "@/lib/utils";

import { ChatGPTAccountPanel } from "./_components/chatgpt";
import { ClaudeCodePanel } from "./_components/claudecode";
import { ProfileRetryFields, RetryPolicyPanel, ZERO_OVERRIDE } from "./_components/retry";

// 思考开关(thinking.type)与思考强度(reasoning_effort)是两个【互相独立】的字段，
// 各自单独设置——有些接口没有 thinking 字段、只靠强度参数就能激活思考，故需解耦。
// 存库空字符串 = 该字段【不发送】；Radix Select 不接受空 value，故 UI 用 "none"
// 哨兵表示不发送，存取时与 "" 互转（NONE / fromStore / toStore）。
const NONE = "none";
const fromStore = (v?: string) => (v ? v : NONE);
const toStore = (v: string) => (v === NONE ? "" : v);

function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return `${secs}s`;
  return `${Math.ceil(secs / 60)}min`;
}

// 一个配置在卡片上显示的「是否正常」。没填 Key 的配置根本发不出请求，比熔断更该先说；
// 其余状态来自轮询的熔断记录（轮询关着时不会产生新记录，此时「正常」= 没有已知故障）。
// 文案走 i18n（llmPage.health.*），所以把 t 传进来由调用方注入。
type Health = { label: string; cls: string; hint?: string };

// claude-code(구독) 경로의 모델 후보. `claude` CLI 에는 모델 목록 조회 명령이 없고 구독 토큰으로 API 의
// /v1/models 를 부르는 것은 약관상 허용되지 않으므로, 실시간 조회 대신 내장 목록을 쓴다. 모델 이름은
// `--model` 로 그대로 전달되며 목록에 없는 값도 직접 입력할 수 있다. 계정이 쓸 수 없는 모델은 연결 테스트에서
// 오류로 드러난다.
const CLAUDE_CODE_MODELS: { value: string; label: string }[] = [
  { value: "claude-fable-5-1", label: "Fable 5.1" },
  { value: "claude-mythos-5-1", label: "Mythos 5.1" },
  { value: "claude-opus-5-5", label: "Opus 5.5" },
  { value: "claude-sonnet-5-5", label: "Sonnet 5.5" },
  { value: "claude-haiku-5-5", label: "Haiku 5.5" },
  { value: "claude-fable-5", label: "Fable 5" },
  { value: "claude-mythos-5", label: "Mythos 5" },
  { value: "opus", label: "alias" },
  { value: "sonnet", label: "alias" },
  { value: "haiku", label: "alias" },
];

type AuthMode = "" | "chatgpt" | "claude-code";

function authModeOf(p?: LLMProfile | null): AuthMode {
  return p?.auth_mode === "chatgpt" || p?.auth_mode === "claude-code" ? p.auth_mode : "";
}

// ─────────────────────────────────────────────────────────────────────────────
// 轮询配置抽屉
// ─────────────────────────────────────────────────────────────────────────────

function PoolSheet({
  open,
  onOpenChange,
  pool,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
}) {
  const t = useTranslations("llmPage");
  const [busy, setBusy] = React.useState(false);

  // 冷却倒计时是后端算出的剩余秒数——抽屉开着且有配置不正常时才定时拉，让它走起来。
  React.useEffect(() => {
    if (!open || !pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    const t = setInterval(() => void onReload(), 10_000);
    return () => clearInterval(t);
  }, [open, pool, onReload]);

  async function toggle(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? t("pool.toast.enabled") : t("pool.toast.disabled"));
      } else {
        toast.success(t("pool.toast.fallbackUpdated"));
      }
    } catch (e) {
      toast.error(t("pool.toast.setFailed", { msg: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? t("pool.toast.recoveredOne") : t("pool.toast.recoveredAll"));
    } catch (e) {
      toast.error(t("pool.toast.recoverFailed", { msg: (e as Error).message }));
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  // 参与轮询的成员（排除被标记「不参与轮询」的），顺序即后端实际的尝试顺序。
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex flex-col gap-0 p-0 data-[side=right]:sm:max-w-lg">
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <ZapIcon className="size-4" /> {t("pool.title")}
          </SheetTitle>
          <SheetDescription>{t.rich("pool.desc", { b: (c) => <b>{c}</b> })}</SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-6">
          <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="grid gap-0.5">
              <Label className="text-sm">{t("pool.enable.label")}</Label>
              <p className="text-muted-foreground text-xs">{t("pool.enable.hint")}</p>
            </div>
            <Switch
              checked={enabled}
              disabled={busy}
              onCheckedChange={(v) => void toggle({ llm_pool_enabled: v })}
              aria-label={t("pool.enable.aria")}
            />
          </div>

          {enabled && (
            <>
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">{t("pool.bindFallback.label")}</Label>
                  <p className="text-muted-foreground text-xs">{t("pool.bindFallback.hint")}</p>
                </div>
                <Switch
                  checked={pool?.bind_fallback ?? false}
                  disabled={busy}
                  onCheckedChange={(v) => void toggle({ llm_pool_bind_fallback: v })}
                  aria-label={t("pool.bindFallback.aria")}
                />
              </div>

              <Separator />

              <div className="grid gap-2">
                <div className="flex items-center justify-between">
                  <Label className="text-sm">{t("pool.order.label")}</Label>
                  {tripped.length > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => void recover()}>
                      <RotateCcwIcon /> {t("pool.recoverAll")}
                    </Button>
                  )}
                </div>
                {inChain.length < 2 && (
                  <p className="text-muted-foreground text-xs">{t("pool.order.tooFew", { n: inChain.length })}</p>
                )}
                {chain.map((m) => {
                  const excluded = m.excluded && !m.active;
                  const order = excluded ? null : inChain.findIndex((x) => x.profile_id === m.profile_id) + 1;
                  return (
                    <div
                      key={m.profile_id}
                      className={cn(
                        "grid gap-1 rounded-lg border p-2.5 text-sm",
                        excluded && "opacity-55",
                        m.state === "tripped" && "border-destructive/40",
                      )}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="w-5 shrink-0 text-center font-mono text-muted-foreground text-xs">
                          {order ?? "—"}
                        </span>
                        <span className="font-medium">{m.name}</span>
                        {m.active && (
                          <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                            {t("pool.badge.active")}
                          </Badge>
                        )}
                        {excluded && <Badge variant="outline">{t("pool.badge.excluded")}</Badge>}
                        <div className="ml-auto flex items-center gap-2">
                          {m.state === "tripped" && m.cooldown_secs > 0 && (
                            <span className="text-muted-foreground text-xs">
                              {t("pool.cooldown", { cooldown: cooldownText(m.cooldown_secs) })}
                            </span>
                          )}
                          {m.state === "degraded" && (
                            <span className="text-muted-foreground text-xs">
                              {t("pool.consecutiveFails", { fails: m.fails })}
                            </span>
                          )}
                          {m.state !== "ok" && (
                            <Button
                              size="icon"
                              variant="ghost"
                              className="size-7"
                              aria-label={t("pool.recoverNow.aria")}
                              title={t("pool.recoverNow.title")}
                              onClick={() => void recover(m.profile_id)}
                            >
                              <RotateCcwIcon className="size-3.5" />
                            </Button>
                          )}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 pl-7 text-muted-foreground text-xs">
                        <code className="truncate font-mono">{m.model}</code>
                        {!m.active && <span>{t("pool.priority", { priority: m.priority })}</span>}
                      </div>
                      {m.last_error && (
                        <p className="truncate pl-7 font-mono text-muted-foreground text-xs" title={m.last_error}>
                          {m.last_error}
                        </p>
                      )}
                    </div>
                  );
                })}
                {chain.length === 0 && (
                  <div className="rounded-lg border border-dashed p-4 text-center text-muted-foreground text-sm">
                    {t("pool.empty")}
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs leading-relaxed">
                {t("pool.note")}
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// 模型配置抽屉（新建 / 编辑共用同一套表单）
// ─────────────────────────────────────────────────────────────────────────────

function ProfileSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null; // null = 新建
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (id: string) => void;
}) {
  const t = useTranslations("llmPage");
  const isNew = !profile;
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<"anthropic" | "openai" | "openai-responses">("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0"); // 上下文窗口(K tokens);0=默认200K
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0"); // 轮询顺位;越大越先
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true); // true=流式(默认);false=非流式
  const [maxTokens, setMaxTokens] = React.useState("0"); // 单次回复输出上限;0=不发送
  const [maxTokensField, setMaxTokensField] = React.useState(NONE); // 上限用哪个字段名;NONE=max_tokens
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState(""); // 自定义会话头名;空=不发送
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE); // 本配置的重试覆盖;全 0=跟随全局
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<{ value: string; label?: string }[]>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);
  // 인증 방식: ""=API Key | "chatgpt"=ChatGPT 구독(Sign in with ChatGPT) | "claude-code"=공식 claude CLI(개인 로컬 사용).
  const [authMode, setAuthMode] = React.useState<AuthMode>("");
  // 새 프로필을 로그인 때문에 먼저 저장했다면 그 id (시트를 닫기 전까지는 이후 저장이 갱신이 된다).
  const [createdId, setCreatedId] = React.useState<number | undefined>(undefined);
  const [chatgptStatus, setChatgptStatus] = React.useState<ChatGPTStatus | null>(null);
  // ChatGPT 구독 전용: OpenAI Daybreak(Trusted Access for Cyber) 켜기/끄기. 기본은 꺼짐.
  const [daybreak, setDaybreak] = React.useState(false);
  // 서버에 저장되어 있는 인증 방식. 로그인을 위해 먼저 저장하면 prop 의 profile 보다 앞서간다.
  const [savedAuthMode, setSavedAuthMode] = React.useState<AuthMode>("");

  // 思考开关 / 上限字段名 / 思考强度三组选项的标签走 i18n，故在组件内按当前语言构造。
  const THINKING_TYPES: { value: string; label: string }[] = [
    { value: NONE, label: t("profile.thinkingType.opt.none") },
    { value: "disabled", label: t("profile.thinkingType.opt.disabled") },
    { value: "enabled", label: t("profile.thinkingType.opt.enabled") },
  ];
  const MAX_TOKENS_FIELDS: { value: string; label: string }[] = [
    { value: NONE, label: t("profile.maxTokensField.opt.default") },
    { value: "max_completion_tokens", label: "max_completion_tokens" },
  ];
  // ChatGPT 구독(Responses API reasoning.effort): thinking.type 은 쓰지 않고 강도만 고른다.
  // 모델마다 지원하는 강도가 다르므로(카탈로그에 목록이 없다) 정적 목록이며, 안 맞는 값은 연결 테스트에서 드러난다.
  const CHATGPT_EFFORT_LEVELS: { value: string; label: string }[] = [
    { value: NONE, label: t("profile.effort.opt.none") },
    { value: "minimal", label: "minimal" },
    { value: "low", label: "low" },
    { value: "medium", label: "medium" },
    { value: "high", label: "high" },
    { value: "xhigh", label: "xhigh" },
  ];
  const EFFORT_LEVELS: { value: string; label: string }[] = [
    { value: NONE, label: t("profile.effort.opt.none") },
    { value: "low", label: "low" },
    { value: "medium", label: "medium" },
    { value: "high", label: "high" },
    { value: "xhigh", label: "xhigh" },
    { value: "max", label: "max" },
  ];

  // 每次打开时从传入的 profile 灌一遍表单（新建则重置为默认值）。抽屉关掉再打开
  // 就是一次干净的开始，不会留下上一个配置的残影。
  React.useEffect(() => {
    if (!open) return;
    setName(profile?.name ?? "");
    setFormat(profile?.format === "openai" || profile?.format === "openai-responses" ? profile.format : "anthropic");
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(String(profile?.context_window_k ?? 0));
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setDaybreak(profile?.daybreak ?? false);
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelsOpen(false);
    setAuthMode(authModeOf(profile));
    setSavedAuthMode(authModeOf(profile));
    setCreatedId(undefined);
    setChatgptStatus(null);
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : createdId;
  const chatgpt = authMode === "chatgpt";
  const claudeCode = authMode === "claude-code";
  // 구독 방식(ChatGPT/claude-code)은 API Key·Base URL·출력 상한·세션 헤더·비스트리밍을 쓰지 않는다.
  const subscription = chatgpt || claudeCode;
  // Daybreak(access_programs)는 Responses API 에 정의돼 있어 ChatGPT 구독과 openai-responses 형식 API Key 프로필에서만 쓴다.
  const daybreakApplies = chatgpt || (!subscription && format === "openai-responses");

  // quiet: 로그인 직후 자동 조회 — 목록 팝오버를 열지 않고 실패도 알리지 않는다.
  async function loadModels(quiet = false) {
    if (loadingModels) return;
    if (chatgpt) {
      if (!profileId) {
        if (!quiet) toast.error(t("chatgpt.toast.loginFirst"));
        return;
      }
      setLoadingModels(true);
      try {
        const r = await api.chatgptModels(profileId);
        if (r.ok && r.models && r.models.length > 0) {
          setModels(
            r.models.map((m) => ({
              value: m.slug,
              label: m.display_name && m.display_name !== m.slug ? m.display_name : undefined,
            })),
          );
          if (!quiet) {
            setModelsOpen(true);
            toast.success(t("profile.toast.modelsLoaded", { n: r.models.length }));
          }
        } else if (!quiet) {
          toast.error(t("profile.toast.loadModelsFailed", { error: r.error ?? t("profile.toast.noModels") }));
        }
      } catch (e) {
        if (!quiet) toast.error(t("profile.toast.loadModelsError", { msg: (e as Error).message }));
      } finally {
        setLoadingModels(false);
      }
      return;
    }
    setLoadingModels(true);
    if (!quiet) setModels([]);
    try {
      const r = await api.fetchLLMModels(format, baseUrl, apiKey, proxy, profileId);
      if (r.ok && r.models && r.models.length > 0) {
        setModels(r.models.map((value) => ({ value })));
        if (!quiet) {
          setModelsOpen(true);
          toast.success(t("profile.toast.modelsLoaded", { n: r.models.length }));
        }
      } else if (!quiet) {
        toast.error(t("profile.toast.loadModelsFailed", { error: r.error ?? t("profile.toast.noModels") }));
      }
    } catch (e) {
      if (!quiet) toast.error(t("profile.toast.loadModelsError", { msg: (e as Error).message }));
    } finally {
      setLoadingModels(false);
    }
  }

  // 저장된 API Key 프로필을 열면(키가 저장돼 있으면) 모델 목록을 조용히 미리 불러온다. 입력한 키로는
  // API Key 칸을 벗어날 때 불러온다(아래 onBlur).
  // biome-ignore lint/correctness/useExhaustiveDependencies: 프로필·형식이 바뀔 때만 다시 불러온다(loadModels 는 매 렌더 새로 만들어짐)
  React.useEffect(() => {
    if (open && !subscription && profileId && keyHint) void loadModels(true);
  }, [open, subscription, profileId, keyHint, format]);

  // 로그인이 연결되고 플랜 사용 권한도 허가된 순간 한 번, 계정의 모델 목록을 조용히 불러온다.
  const chatgptReady = chatgpt && chatgptStatus?.status === "connected" && chatgptStatus.plan_usage;
  // biome-ignore lint/correctness/useExhaustiveDependencies: 연결 상태가 바뀔 때만 한 번 불러온다(loadModels 는 매 렌더 새로 만들어짐)
  React.useEffect(() => {
    if (chatgptReady) void loadModels(true);
  }, [chatgptReady]);

  async function testConnection() {
    if (testing) return;
    if (chatgpt && !profileId) {
      toast.error(t("chatgpt.toast.testFirst"));
      return;
    }
    setTesting(true);
    try {
      // claude-code: 사용자가 로그인해 둔 claude CLI 를 실제로 한 번 구동해 짧은 응답을 받는다(모델·강도는 폼 값).
      // ChatGPT 구독: 저장된 프로필의 로그인으로 실제 최소 호출을 보낸다(모델·강도는 폼 값).
      const r = claudeCode
        ? await api.testLLM(
            "anthropic",
            model.trim(),
            "",
            "",
            proxy,
            "",
            toStore(effort),
            profileId,
            true,
            "",
            "claude-code",
          )
        : chatgpt
          ? await api.testLLM(
              "openai-responses",
              model,
              "",
              "",
              proxy,
              "",
              toStore(effort),
              profileId,
              true,
              "",
              "chatgpt",
              daybreak,
            )
          : await testPlainConnection();
      showTestResult(r);
    } catch (e) {
      toast.error(t("profile.toast.testError", { msg: (e as Error).message }));
    } finally {
      setTesting(false);
    }
  }

  function showTestResult(r: { ok: boolean; error?: string; latency_ms?: number; model?: string; reply?: string }) {
    // 回复内容一并展示：看得见模型确实说了话，才算和会话里跑通是一回事。
    if (r.ok)
      toast.success(t("profile.toast.testOk", { latency: r.latency_ms ?? "?", model: r.model ?? model }), {
        description: r.reply ? t("profile.toast.testReply", { reply: r.reply }) : undefined,
      });
    else toast.error(t("profile.toast.testFailed", { error: r.error ?? t("profile.toast.unknown") }));
  }

  function testPlainConnection() {
    // 用配置实际会跑的思考参数来测，这样不支持该字段的模型在这里就失败，
    // 而不是等到跑任务时才炸。传 profile id：Key 输入框留空时用已存的 Key。
    return api.testLLM(
      format,
      model,
      baseUrl,
      apiKey,
      proxy,
      toStore(thinkingType),
      toStore(effort),
      profileId,
      streaming,
      sessionHeaderKey.trim(),
      "",
      daybreakApplies && daybreak,
    );
  }

  // 폼을 저장해 프로필 id 를 돌려준다. 검증에 실패하면 알리고 null. 로그인처럼 저장 전에 id 가
  // 필요한 동작도 이 함수를 쓴다(새 프로필이면 createdId 를 기억해 이후 저장이 갱신이 되게 한다).
  async function persist(): Promise<number | null> {
    // claude-code 는 모델을 비워 두면 claude 의 기본 모델을 쓴다.
    if (!name.trim() || (!claudeCode && !model.trim())) {
      toast.error(t(claudeCode ? "claudeCode.toast.nameRequired" : "profile.toast.nameModelRequired"));
      return null;
    }
    if (authMode === "" && savedAuthMode === "chatgpt" && !apiKey.trim()) {
      // 구독 → API Key 로 바꾸면 ChatGPT 로그인이 폐기되므로 키가 반드시 필요하다.
      toast.error(t("profile.toast.apiKeyRequiredSwitch"));
      return null;
    }
    const existingId = profile ? Number(profile.id) : createdId;
    const { id } = await api.saveLLMProfile({
      ...(existingId ? { id: existingId } : {}),
      name: name.trim(),
      // 구독 프로필은 서버가 형식·Base URL·키·출력 상한·세션 헤더·스트리밍을 정규화한다.
      // 화면이 모순된 값을 보내지 않도록 같은 값으로 맞춰 보낸다.
      format: chatgpt ? "openai-responses" : claudeCode ? "anthropic" : format,
      model: model.trim(),
      base_url: subscription ? "" : baseUrl.trim(),
      proxy: proxy.trim(),
      api_key: subscription ? "" : apiKey,
      // claude-code 는 속도 제한(rate_per_*)을 쓰지 않는다(동시 실행 상한이 대신한다).
      rate_per_second: claudeCode ? 0 : Number(rps) || 0,
      rate_per_minute: claudeCode ? 0 : Number(rpm) || 0,
      context_window_k: Number(cw) || 0,
      thinking_type: subscription ? "" : toStore(thinkingType),
      reasoning_effort: toStore(effort),
      priority: Number(priority) || 0,
      pool_exclude: poolExclude,
      streaming: subscription ? true : streaming,
      max_tokens: subscription ? 0 : Math.max(0, Number(maxTokens) || 0),
      // 字段名开关只对 openai(Chat Completions) 有意义，其它格式一律回落到默认；
      // 后端也会再做一次同样的归一化，这里只是别让 UI 送出自相矛盾的值。
      max_tokens_field: !subscription && format === "openai" ? toStore(maxTokensField) : "",
      session_header_key: subscription ? "" : sessionHeaderKey.trim(),
      retry,
      auth_mode: authMode,
      daybreak: daybreakApplies && daybreak,
    });
    if (!existingId) setCreatedId(id);
    setSavedAuthMode(authMode);
    return id;
  }

  // 로그인 버튼용: 저장하고 id 를 돌려준다(목록도 갱신).
  async function ensureSaved(): Promise<number | null> {
    try {
      const id = await persist();
      if (id != null) onSaved(String(id));
      return id;
    } catch (e) {
      toast.error(t("profile.toast.saveFailed", { msg: (e as Error).message }));
      return null;
    }
  }

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      const id = await persist();
      if (id == null) return;
      if (isNew && !createdId) toast.success(t("profile.toast.created", { name: name.trim() }));
      else toast.success(profile?.is_default ? t("profile.toast.savedActive") : t("profile.toast.saved"));
      onSaved(String(id));
      onOpenChange(false);
    } catch (e) {
      toast.error(t("profile.toast.saveFailed", { msg: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? t("profile.titleNew") : t("profile.titleEdit", { name: profile?.name ?? "" })}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                {t("profile.activeBadge")}
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>{isNew ? t("profile.descNew") : t("profile.descEdit")}</SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-name">{t("profile.name.label")}</Label>
              <Input
                id="p-name"
                placeholder={t("profile.name.placeholder")}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label>{t("profile.authMode.label")}</Label>
              <Select
                value={authMode === "" ? "apikey" : authMode}
                onValueChange={(v) => {
                  setAuthMode(v === "chatgpt" || v === "claude-code" ? v : "");
                  // 구독 방식이 지원하지 않는 강도(max)는 기본값으로 돌린다.
                  if (v === "chatgpt" && !CHATGPT_EFFORT_LEVELS.some((o) => o.value === effort)) setEffort(NONE);
                  if (v === "claude-code" && !EFFORT_LEVELS.some((o) => o.value === effort)) setEffort(NONE);
                  setModels([]);
                }}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="apikey">{t("profile.authMode.apikey")}</SelectItem>
                  <SelectItem value="chatgpt">{t("profile.authMode.chatgpt")}</SelectItem>
                  <SelectItem value="claude-code">{t("profile.authMode.claudeCode")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          {!subscription && (
            <div className="grid gap-2">
              <Label>{t("profile.format.label")}</Label>
              <Select value={format} onValueChange={(v) => setFormat(v as "anthropic" | "openai" | "openai-responses")}>
                <SelectTrigger>
                  <SelectValue placeholder={t("profile.format.placeholder")} />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">Anthropic</SelectItem>
                  <SelectItem value="openai">OpenAI (Chat Completions)</SelectItem>
                  <SelectItem value="openai-responses">OpenAI (Responses API)</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}

          {chatgpt && (
            <ChatGPTAccountPanel profileId={profileId} ensureSaved={ensureSaved} onStatus={setChatgptStatus} />
          )}
          {claudeCode && <ClaudeCodePanel />}

          <div className="grid gap-2">
            <Label htmlFor="p-model">{t("profile.model.label")}</Label>
            <div className="flex gap-2">
              <Input
                id="p-model"
                className="font-mono"
                placeholder={claudeCode ? t("claudeCode.modelPlaceholder") : t("profile.model.placeholder")}
                value={model}
                onChange={(e) => setModel(e.target.value)}
                // 브라우저 기본 자동 완성 목록: 칸을 누르거나 입력하면 불러온 모델(claude-code 는 내장 목록)이 펼쳐지고,
                // 목록에 없는 값도 그대로 입력할 수 있다.
                list="p-model-options"
                autoComplete="off"
              />
              <datalist id="p-model-options">
                {(claudeCode ? CLAUDE_CODE_MODELS : models).map((m) => (
                  <option key={m.value} value={m.value} label={m.label} />
                ))}
              </datalist>
              {/* modal: 这个 Popover 的内容被 portal 到 <body>，在 Sheet 的滚动锁之外，
                  不加 modal 时列表能渲染却滚不动。modal 让它自己持有最上层滚动锁。 */}
              {claudeCode ? (
                <Popover open={modelsOpen} onOpenChange={setModelsOpen} modal>
                  <PopoverTrigger asChild>
                    <Button type="button" variant="outline" className="shrink-0" title={t("claudeCode.modelListTitle")}>
                      <ListIcon />
                      {t("profile.model.listButton")}
                    </Button>
                  </PopoverTrigger>
                  <PopoverContent className="max-h-80 w-72 gap-0 overflow-y-auto overscroll-contain p-1" align="end">
                    {CLAUDE_CODE_MODELS.map((m) => (
                      <button
                        key={m.value}
                        type="button"
                        className="w-full shrink-0 rounded-md px-2 py-1.5 text-left font-mono text-xs hover:bg-accent hover:text-accent-foreground"
                        onClick={() => {
                          setModel(m.value);
                          setModelsOpen(false);
                        }}
                      >
                        {m.value}
                        <span className="block font-sans text-muted-foreground">{m.label}</span>
                      </button>
                    ))}
                    <p className="px-2 py-1.5 font-sans text-muted-foreground text-xs">
                      {t("claudeCode.modelListNote")}
                    </p>
                  </PopoverContent>
                </Popover>
              ) : (
                <Popover open={modelsOpen} onOpenChange={setModelsOpen} modal>
                  <PopoverTrigger asChild>
                    <Button
                      type="button"
                      variant="outline"
                      className="shrink-0"
                      disabled={loadingModels}
                      onClick={() => void loadModels()}
                      title={t("profile.model.loadTitle")}
                    >
                      {loadingModels ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                      {t("profile.model.listButton")}
                    </Button>
                  </PopoverTrigger>
                  {models.length > 0 && (
                    <PopoverContent className="max-h-72 w-72 gap-0 overflow-y-auto overscroll-contain p-1" align="end">
                      {models.map((m) => (
                        <button
                          key={m.value}
                          type="button"
                          className="w-full shrink-0 rounded-md px-2 py-1.5 text-left font-mono text-xs hover:bg-accent hover:text-accent-foreground"
                          onClick={() => {
                            setModel(m.value);
                            setModelsOpen(false);
                          }}
                        >
                          {m.value}
                          {m.label && <span className="block font-sans text-muted-foreground">{m.label}</span>}
                        </button>
                      ))}
                    </PopoverContent>
                  )}
                </Popover>
              )}
            </div>
            {chatgpt && <p className="text-muted-foreground text-xs">{t("chatgpt.modelHint")}</p>}
            {claudeCode && <p className="text-muted-foreground text-xs">{t("claudeCode.modelHint")}</p>}
            {!subscription && <p className="text-muted-foreground text-xs">{t("profile.model.hint")}</p>}
          </div>

          {!subscription && (
            <div className="grid gap-2">
              <Label htmlFor="p-base-url">{t("profile.baseUrl.label")}</Label>
              <Input
                id="p-base-url"
                className="font-mono"
                placeholder="https://api.openai.com/v1"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
              />
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="p-proxy">{t("profile.proxy.label")}</Label>
            <Input
              id="p-proxy"
              className="font-mono"
              placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">{t("profile.proxy.hint")}</p>
          </div>

          {!subscription && (
            <>
              <div className="grid gap-2">
                <Label htmlFor="p-session-header">{t("profile.sessionHeader.label")}</Label>
                <Input
                  id="p-session-header"
                  className="font-mono"
                  placeholder={t("profile.sessionHeader.placeholder")}
                  value={sessionHeaderKey}
                  onChange={(e) => setSessionHeaderKey(e.target.value)}
                />
                <p className="text-muted-foreground text-xs">
                  {t.rich("profile.sessionHeader.hint", { b: (c) => <b>{c}</b> })}
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="p-api-key">API Key</Label>
                <Input
                  id="p-api-key"
                  type="password"
                  placeholder={keyHint ? t("profile.apiKey.placeholderSet", { hint: keyHint }) : "sk-…"}
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  onBlur={() => {
                    if (apiKey.trim()) void loadModels(true);
                  }}
                />
              </div>
            </>
          )}

          <div className={cn("grid gap-4", claudeCode ? "sm:grid-cols-1" : "sm:grid-cols-3")}>
            {!claudeCode && (
              <>
                <div className="grid gap-2">
                  <Label htmlFor="p-rps">{t("profile.rps")}</Label>
                  <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="p-rpm">{t("profile.rpm")}</Label>
                  <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
                </div>
              </>
            )}
            <div className="grid gap-2">
              <Label htmlFor="p-cw">{t("profile.cw")}</Label>
              <Input
                id="p-cw"
                type="number"
                min={0}
                max={1000}
                value={cw}
                onChange={(e) => setCw(e.target.value)}
                placeholder="200"
              />
            </div>
          </div>
          {!claudeCode && <p className="-mt-2 text-muted-foreground text-xs">{t("profile.rateHint")}</p>}

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-priority" className="text-sm">
                  {t("profile.priority.label")}
                </Label>
                <p className="text-muted-foreground text-xs">{t("profile.priority.hint")}</p>
              </div>
              <Input
                id="p-priority"
                type="number"
                className="w-24 shrink-0"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">{t("profile.exclude.label")}</Label>
                <p className="text-muted-foreground text-xs">{t("profile.exclude.hint")}</p>
              </div>
              <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label={t("profile.exclude.aria")} />
            </div>
            {!subscription && (
              <div className="flex items-center justify-between gap-4 border-t pt-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">{t("profile.streaming.label")}</Label>
                  <p className="text-muted-foreground text-xs">{t("profile.streaming.hint")}</p>
                </div>
                <Switch checked={streaming} onCheckedChange={setStreaming} aria-label={t("profile.streaming.aria")} />
              </div>
            )}
          </div>

          {!subscription && (
            <div className="grid gap-3 rounded-lg border p-3">
              <div className="flex items-center justify-between gap-4">
                <div className="grid gap-0.5">
                  <Label htmlFor="p-max-tokens" className="text-sm">
                    {t("profile.maxTokens.label")}
                  </Label>
                  <p className="text-muted-foreground text-xs">{t("profile.maxTokens.hint")}</p>
                </div>
                <Input
                  id="p-max-tokens"
                  type="number"
                  min={0}
                  className="w-28 shrink-0"
                  value={maxTokens}
                  onChange={(e) => setMaxTokens(e.target.value)}
                  placeholder="0"
                />
              </div>
              <div className="flex items-center justify-between gap-4 border-t pt-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">{t("profile.maxTokensField.label")}</Label>
                  <p className="text-muted-foreground text-xs">{t(`profile.maxTokensFieldHint.${format}`)}</p>
                </div>
                <Select
                  value={format === "openai" ? maxTokensField : NONE}
                  onValueChange={setMaxTokensField}
                  disabled={format !== "openai"}
                >
                  <SelectTrigger className="w-56 shrink-0">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {MAX_TOKENS_FIELDS.map((o) => (
                      <SelectItem key={o.value} value={o.value}>
                        {o.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
          )}

          <div className="grid gap-3 rounded-lg border p-3">
            {!subscription && (
              <div className="flex items-center justify-between gap-4">
                <div className="grid gap-0.5">
                  <Label className="text-sm">{t("profile.thinkingType.label")}</Label>
                  <p className="text-muted-foreground text-xs">{t("profile.thinkingType.hint")}</p>
                </div>
                <Select value={thinkingType} onValueChange={setThinkingType}>
                  <SelectTrigger className="w-32 shrink-0">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {THINKING_TYPES.map((o) => (
                      <SelectItem key={o.value} value={o.value}>
                        {o.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
            <div className={cn("flex items-center justify-between gap-4", !subscription && "border-t pt-3")}>
              <div className="grid gap-0.5">
                <Label className="text-sm">{t("profile.effort.label")}</Label>
                <p className="text-muted-foreground text-xs">
                  {chatgpt
                    ? t("chatgpt.effortHint")
                    : claudeCode
                      ? t("claudeCode.effortHint")
                      : t("profile.effort.hint")}
                </p>
              </div>
              <Select value={effort} onValueChange={setEffort}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(chatgpt ? CHATGPT_EFFORT_LEVELS : EFFORT_LEVELS).map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {daybreakApplies && (
              <div className="flex items-center justify-between gap-4 border-t pt-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">{t("chatgpt.daybreakLabel")}</Label>
                  <p className="text-muted-foreground text-xs">{t("chatgpt.daybreakHint")}</p>
                </div>
                <Switch checked={daybreak} onCheckedChange={setDaybreak} aria-label={t("chatgpt.daybreakAria")} />
              </div>
            )}
          </div>

          <ProfileRetryFields value={retry} onChange={setRetry} />
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? t("profile.testing") : t("profile.testConnection")}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? t("profile.create") : t("profile.save")}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────

export default function LLMPage() {
  const t = useTranslations("llmPage");
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [poolOpen, setPoolOpen] = React.useState(false);
  // 抽屉的开关和内容分开存：关闭时 editing 保持不变，否则关闭动画期间标题会从
  // 「编辑 X」闪成「新建」。editing = null 表示新建。
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  // claude-code 프로필이 있을 때만 설치 단위 스위치를 읽는다(null=아직 모름). 꺼져 있으면 카드에 알린다.
  const [claudeCodeOn, setClaudeCodeOn] = React.useState<boolean | null>(null);
  const hasClaudeCode = profiles.some((p) => p.auth_mode === "claude-code");
  React.useEffect(() => {
    if (!hasClaudeCode) return;
    let cancelled = false;
    api
      .claudeCodeSettings()
      .then((s) => !cancelled && setClaudeCodeOn(s.effective_enabled))
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [hasClaudeCode]);

  // 一个配置在卡片上显示的「是否正常」。没填 Key 的配置根本发不出请求，比熔断更该先说。
  const healthOf = React.useCallback(
    (p: LLMProfile, m?: LLMPoolMember): Health => {
      if (p.auth_mode === "claude-code") {
        // claude-code 는 Key 가 없고 설치 단위 스위치가 "호출할 수 있는가"를 정한다(로그인은 claude 가 안다).
        if (claudeCodeOn === false) {
          return {
            label: t("health.claudeCodeOff.label"),
            cls: "border-muted-foreground/40 text-muted-foreground",
            hint: t("health.claudeCodeOff.hint"),
          };
        }
      } else if (p.auth_mode === "chatgpt") {
        // 구독 프로필은 Key 대신 로그인 상태가 "호출할 수 있는가"를 정한다.
        const c = p.chatgpt;
        if (!c || c.status === "" || c.status === "disconnected") {
          return {
            label: t("health.chatgptLogin.label"),
            cls: "border-muted-foreground/40 text-muted-foreground",
            hint: t("health.chatgptLogin.hint"),
          };
        }
        if (c.status === "reauth_required") {
          return {
            label: t("health.chatgptReauth.label"),
            cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
            hint: t("health.chatgptReauth.hint"),
          };
        }
        if (!c.plan_usage) {
          return {
            label: t("health.chatgptNoPlan.label"),
            cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
            hint: t("health.chatgptNoPlan.hint"),
          };
        }
      } else if (!p.api_key_hint) {
        return {
          label: t("health.noKey.label"),
          cls: "border-muted-foreground/40 text-muted-foreground",
          hint: t("health.noKey.hint"),
        };
      }
      if (m?.state === "tripped") {
        return {
          label:
            m.cooldown_secs > 0
              ? t("health.trippedCooldown", { cooldown: cooldownText(m.cooldown_secs) })
              : t("health.tripped"),
          cls: "border-destructive/50 text-destructive",
          hint: m.last_error,
        };
      }
      if (m?.state === "degraded") {
        return {
          label: t("health.degraded", { fails: m.fails }),
          cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
          hint: m.last_error,
        };
      }
      return { label: t("health.ok"), cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };
    },
    [t, claudeCodeOn],
  );

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* ignore */
    }
  }, []);

  const load = React.useCallback(async () => {
    try {
      setProfiles(await api.llmProfiles());
    } catch {
      /* ignore */
    }
    await loadPool();
  }, [loadPool]);

  React.useEffect(() => {
    void load();
  }, [load]);

  // 卡片上的健康徽章按 profile id 取轮询状态。
  const health = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(t("page.toast.activated", { name }));
      await load();
    } catch (e) {
      toast.error(t("page.toast.activateFailed", { msg: (e as Error).message }));
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error(t("page.toast.cannotDeleteActive"));
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(t("page.toast.deleted", { name: p.name }));
      await load();
    } catch (e) {
      toast.error(t("page.toast.deleteFailed", { msg: (e as Error).message }));
    }
  }

  const poolOn = pool?.enabled ?? false;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">LLM</h1>
          <p className="text-muted-foreground text-sm">{t("page.subtitle")}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => setPoolOpen(true)}>
            <ZapIcon /> {t("page.poolConfig")}
            {poolOn && (
              <Badge variant="outline" className="ml-1 border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                {t("page.enabled")}
              </Badge>
            )}
          </Button>
          <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
            <PlusIcon /> {t("page.new")}
          </Button>
        </div>
      </div>

      <Tabs defaultValue="profiles" className="flex-1">
        <TabsList>
          <TabsTrigger value="profiles">{t("page.tab.profiles")}</TabsTrigger>
          <TabsTrigger value="retry">{t("page.tab.retry")}</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4">
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {profiles.map((p) => {
              const h = healthOf(p, health.get(p.id));
              return (
                // biome-ignore lint/a11y/useSemanticElements: 卡片内含自己的操作按钮，用原生 <button> 会造成按钮嵌套（非法 HTML）
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30",
                    p.is_default && "border-amber-400/50 bg-amber-400/5",
                  )}
                >
                  <CardContent className="grid gap-2 px-4">
                    <div className="flex items-start gap-2">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className={p.auth_mode ? undefined : "uppercase"}>
                            {p.auth_mode === "chatgpt"
                              ? t("card.chatgptBadge")
                              : p.auth_mode === "claude-code"
                                ? t("card.claudeCodeBadge")
                                : p.format}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto", h.cls)} title={h.hint}>
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">
                          {p.model || (p.auth_mode === "claude-code" ? t("card.defaultModel") : "")}
                        </code>
                      </div>
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6 text-muted-foreground text-xs">
                      {p.auth_mode === "chatgpt"
                        ? p.chatgpt?.email && <span className="truncate">{p.chatgpt.email}</span>
                        : p.auth_mode !== "claude-code" && p.api_key_hint && <span>{p.api_key_hint}</span>}
                      {p.auth_mode !== "claude-code" && (
                        <span>
                          {p.rate_per_second}/s · {p.rate_per_minute}/min
                        </span>
                      )}
                      {p.proxy && <span className="truncate">{t("card.proxy", { proxy: p.proxy })}</span>}
                      {p.daybreak && <span>{t("chatgpt.daybreakLabel")}</span>}
                      {p.reasoning_effort && (
                        <span>
                          {t("card.thinking", {
                            effort: p.reasoning_effort === "off" ? t("card.thinkingOff") : p.reasoning_effort,
                          })}
                        </span>
                      )}
                      {/* 轮询相关的两个字段只在轮询开着时才有意义，关着时不占版面 */}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? (
                          <span>{t("card.excluded")}</span>
                        ) : (
                          <span>{t("card.priority", { priority: p.priority ?? 0 })}</span>
                        ))}
                    </div>

                    <div className="mt-1 flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? t("card.active") : t("card.activate")}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label={t("card.deleteAria")}
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
            {profiles.length === 0 && (
              <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
                {t("page.empty")}
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileSheet profile={editing} open={editOpen} onOpenChange={setEditOpen} onSaved={() => void load()} />
      <PoolSheet open={poolOpen} onOpenChange={setPoolOpen} pool={pool} onReload={loadPool} />
    </div>
  );
}
