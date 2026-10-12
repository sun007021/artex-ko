"use client";

import * as React from "react";

import { AlertTriangleIcon, ExternalLinkIcon, Loader2Icon, RefreshCwIcon, TerminalIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { ClaudeCodeProbe, ClaudeCodeSettings } from "@/lib/types";
import { cn } from "@/lib/utils";

// claude-code 인증 방식 패널. ARTEX 는 Claude 자격 증명을 읽거나 저장하거나 전달하지 않는다 — 로그인은
// 사용자가 터미널에서 직접 하고, 이 패널은 `claude auth status` 의 결과(로그인 여부와 방식)만 보여 준다.

const SOURCES: { key: "legal" | "auth" | "headless" | "cli" | "terms"; href: string }[] = [
  { key: "legal", href: "https://code.claude.com/docs/en/legal-and-compliance" },
  { key: "auth", href: "https://code.claude.com/docs/en/authentication" },
  { key: "headless", href: "https://code.claude.com/docs/en/headless" },
  { key: "cli", href: "https://code.claude.com/docs/en/cli-reference" },
  { key: "terms", href: "https://www.anthropic.com/legal/consumer-terms" },
];

const OK_CLS = "border-emerald-500/50 text-emerald-600 dark:text-emerald-400";
const WARN_CLS = "border-amber-500/50 text-amber-600 dark:text-amber-400";
const NEUTRAL_CLS = "border-muted-foreground/40 text-muted-foreground";

export function ClaudeCodePanel() {
  const t = useTranslations("llmPage.claudeCode");
  const [settings, setSettings] = React.useState<ClaudeCodeSettings | null>(null);
  const [probe, setProbe] = React.useState<ClaudeCodeProbe | null>(null);
  const [checking, setChecking] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  // 폼 값(저장 전). 저장된 값과 다르면 "적용" 을 눌러야 서버에 반영된다.
  const [enabled, setEnabled] = React.useState(false);
  const [binary, setBinary] = React.useState("");
  const [concurrent, setConcurrent] = React.useState("1");
  const [ack, setAck] = React.useState(false);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  const applySettings = React.useCallback((s: ClaudeCodeSettings) => {
    setSettings(s);
    setEnabled(s.enabled);
    setBinary(s.binary_path);
    setConcurrent(String(s.max_concurrent));
  }, []);

  const check = React.useCallback(async () => {
    setChecking(true);
    try {
      const r = await api.claudeCodeStatus();
      setProbe(r.status);
      applySettings(r.settings);
      setLoadError(null);
    } catch (e) {
      setLoadError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }, [applySettings]);

  React.useEffect(() => {
    void check();
  }, [check]);

  async function apply() {
    if (saving || !settings) return;
    const turningOn = enabled && !settings.enabled;
    if (turningOn && !ack) {
      toast.error(t("toast.needAck"));
      return;
    }
    setSaving(true);
    try {
      const s = await api.saveClaudeCodeSettings({
        enabled,
        binary_path: binary.trim(),
        max_concurrent: Number(concurrent) || 1,
        acknowledged: turningOn ? ack : undefined,
      });
      applySettings(s);
      setAck(false);
      toast.success(t("toast.applied"));
      void check(); // 바이너리 경로가 바뀌었을 수 있으니 상태를 다시 읽는다
    } catch (e) {
      toast.error(t("toast.applyFailed", { msg: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  const dirty =
    settings != null &&
    (enabled !== settings.enabled ||
      binary.trim() !== settings.binary_path ||
      Number(concurrent) !== settings.max_concurrent);
  const hard = settings?.max_concurrent_hard ?? 3;
  const notLoopback = settings != null && !settings.loopback_only;

  let badge = { label: t("status.checking"), cls: NEUTRAL_CLS };
  if (probe) {
    if (probe.usable) badge = { label: t("status.usable"), cls: OK_CLS };
    else
      badge = {
        label: t(`status.problem.${probe.problem || "unknown"}`),
        cls: probe.problem === "not_logged_in" ? NEUTRAL_CLS : WARN_CLS,
      };
  }

  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <TerminalIcon className="size-4 text-muted-foreground" />
        <span className="font-medium text-sm">{t("title")}</span>
        <Badge variant="outline" className={cn("ml-auto", badge.cls)}>
          {checking && <Loader2Icon className="animate-spin" />}
          {badge.label}
        </Badge>
      </div>
      <p className="text-muted-foreground text-xs leading-relaxed">{t("intro")}</p>

      <Alert>
        <AlertTriangleIcon />
        <AlertTitle>{t("notice.title")}</AlertTitle>
        <AlertDescription>
          <ul className="mt-1 list-disc space-y-1 pl-4 text-xs leading-relaxed">
            <li>{t("notice.personal")}</li>
            <li>{t("notice.noExpose")}</li>
            <li>{t("notice.authorized")}</li>
            <li>{t("notice.noBypass")}</li>
            <li>{t("notice.enforce")}</li>
            <li>{t("notice.env")}</li>
          </ul>
          <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1">
            {SOURCES.map((s) => (
              <a
                key={s.key}
                href={s.href}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-primary text-xs underline underline-offset-2"
              >
                <ExternalLinkIcon className="size-3" /> {t(`sources.${s.key}`)}
              </a>
            ))}
          </div>
        </AlertDescription>
      </Alert>

      {loadError && <p className="text-destructive text-xs">{loadError}</p>}

      {settings && (
        <div className="grid gap-3 rounded-md border border-dashed p-3">
          <div className="flex items-center justify-between gap-4">
            <div className="grid gap-0.5">
              <Label className="text-sm">{t("settings.enable")}</Label>
              <p className="text-muted-foreground text-xs">{t("settings.enableHint")}</p>
            </div>
            <Switch
              checked={enabled}
              onCheckedChange={setEnabled}
              disabled={notLoopback && !settings.enabled}
              aria-label={t("settings.enable")}
            />
          </div>
          {notLoopback && <p className="text-destructive text-xs">{t("settings.notLoopback")}</p>}
          {enabled && !settings.enabled && (
            <label className="flex items-start gap-2 text-xs leading-relaxed">
              <Checkbox checked={ack} onCheckedChange={(v) => setAck(v === true)} className="mt-0.5" />
              <span>{t("settings.ack")}</span>
            </label>
          )}
          <div className="grid gap-2">
            <Label htmlFor="cc-binary" className="text-sm">
              {t("settings.binary")}
            </Label>
            <Input
              id="cc-binary"
              className="font-mono"
              placeholder="claude"
              value={binary}
              onChange={(e) => setBinary(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">{t("settings.binaryHint")}</p>
          </div>
          <div className="flex items-center justify-between gap-4">
            <div className="grid gap-0.5">
              <Label className="text-sm">{t("settings.concurrent")}</Label>
              <p className="text-muted-foreground text-xs">{t("settings.concurrentHint", { max: hard })}</p>
            </div>
            <Select value={concurrent} onValueChange={setConcurrent}>
              <SelectTrigger className="w-20 shrink-0">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {Array.from({ length: hard }, (_, i) => String(i + 1)).map((n) => (
                  <SelectItem key={n} value={n}>
                    {n}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex justify-end">
            <Button type="button" size="sm" onClick={() => void apply()} disabled={saving || !dirty}>
              {saving && <Loader2Icon className="animate-spin" />}
              {t("settings.apply")}
            </Button>
          </div>
        </div>
      )}

      <div className="grid gap-2 rounded-md border border-dashed p-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium text-xs">{t("status.title")}</span>
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="ml-auto"
            onClick={() => void check()}
            disabled={checking}
          >
            {checking ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
            {t("status.check")}
          </Button>
        </div>
        {probe?.installed && (
          <p className="text-xs">
            <span className="text-muted-foreground">{t("status.version")}</span>{" "}
            <span className="font-mono">{probe.version || "?"}</span>
            {probe.logged_in && probe.auth_method && (
              <>
                {" · "}
                <span className="text-muted-foreground">{t("status.method")}</span>{" "}
                <span className="font-mono">{probe.auth_method}</span>
              </>
            )}
          </p>
        )}
        {probe && !probe.usable && probe.problem && (
          <p
            className={cn(
              "text-xs",
              probe.problem === "api_billing" ? "text-destructive" : "text-amber-600 dark:text-amber-400",
            )}
          >
            {t(`status.hint.${probe.problem}`)}
          </p>
        )}
        {(!probe || probe.problem === "not_logged_in" || probe.problem === "not_installed") && (
          <p className="text-muted-foreground text-xs leading-relaxed">
            {t("status.loginHow")} <code className="rounded bg-muted px-1 font-mono">claude auth login</code>
          </p>
        )}
      </div>

      <p className="text-muted-foreground text-xs leading-relaxed">{t("limits")}</p>
    </div>
  );
}
