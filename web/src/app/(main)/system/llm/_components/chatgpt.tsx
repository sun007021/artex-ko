"use client";

import * as React from "react";

import { ExternalLinkIcon, Loader2Icon, LogInIcon, LogOutIcon, ShieldCheckIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, backendUrl } from "@/lib/api";
import type { ChatGPTStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

// ChatGPT 구독(Sign in with ChatGPT) 계정 패널. 로그인은 OpenAI 공식 흐름(브라우저에서 승인 →
// 127.0.0.1 콜백)을 쓰고, 토큰은 서버에만 저장된다. 이 컴포넌트는 토큰을 다루지 않는다.

// 로그인 승인이 끝나기를 기다리며 상태를 다시 읽는 간격.
const POLL_MS = 2000;

export function ChatGPTAccountPanel({
  profileId,
  ensureSaved,
  onStatus,
}: {
  /** 저장된 프로필 id. 아직 저장 전이면 undefined. */
  profileId: number | undefined;
  /** 폼을 먼저 저장하고 프로필 id 를 돌려준다(검증 실패 시 null). */
  ensureSaved: () => Promise<number | null>;
  onStatus?: (s: ChatGPTStatus | null) => void;
}) {
  const t = useTranslations("llmPage.chatgpt");
  const [status, setStatus] = React.useState<ChatGPTStatus | null>(null);
  const [authUrl, setAuthUrl] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState<"login" | "logout" | "paste" | null>(null);
  const [paste, setPaste] = React.useState("");

  const onStatusRef = React.useRef(onStatus);
  onStatusRef.current = onStatus;
  const prevLogin = React.useRef<string>("none");

  const refresh = React.useCallback(async (id: number) => {
    try {
      const s = await api.chatgptStatus(id);
      setStatus(s);
      onStatusRef.current?.(s);
      return s;
    } catch {
      // 프로필이 아직 구독 방식으로 저장되지 않은 경우(404) 등 — 로그인 안 됨으로 둔다.
      setStatus(null);
      onStatusRef.current?.(null);
      return null;
    }
  }, []);

  React.useEffect(() => {
    if (profileId) void refresh(profileId);
  }, [profileId, refresh]);

  const pending = status?.login.state === "pending";
  React.useEffect(() => {
    if (!pending || !profileId) return;
    const h = setInterval(() => void refresh(profileId), POLL_MS);
    return () => clearInterval(h);
  }, [pending, profileId, refresh]);

  // 진행 중이던 로그인이 끝났을 때 한 번만 알린다(콜백 탭에서 끝났든 붙여넣기로 끝났든).
  React.useEffect(() => {
    const cur = status?.login.state ?? "none";
    if (prevLogin.current === "pending") {
      if (cur === "done") toast.success(t("toast.loginDone", { email: status?.email ?? "" }));
      else if (cur === "error") toast.error(t("toast.loginFailed", { msg: status?.login.error ?? "" }));
    }
    prevLogin.current = cur;
  }, [status, t]);

  async function login(reconsent = false) {
    if (busy) return;
    setBusy("login");
    try {
      const id = await ensureSaved();
      if (id == null) return;
      const r = await api.chatgptLoginStart(id, reconsent);
      const url = backendUrl(r.auth_url);
      setAuthUrl(url);
      // 팝업 차단으로 열리지 않을 수 있어, 아래에 같은 링크를 계속 보여 준다.
      window.open(url, "_blank", "noopener,noreferrer");
      await refresh(id);
    } catch (e) {
      toast.error(t("toast.loginFailed", { msg: (e as Error).message }));
    } finally {
      setBusy(null);
    }
  }

  async function submitPaste() {
    if (!profileId || !paste.trim() || busy) return;
    setBusy("paste");
    try {
      const s = await api.chatgptLoginComplete(profileId, paste.trim());
      setPaste("");
      setStatus(s);
      onStatusRef.current?.(s);
      prevLogin.current = "none"; // 완료 알림은 아래에서 직접 낸다
      toast.success(t("toast.loginDone", { email: s.email ?? "" }));
    } catch (e) {
      toast.error(t("toast.pasteFailed", { msg: (e as Error).message }));
    } finally {
      setBusy(null);
    }
  }

  async function logout() {
    if (!profileId || busy) return;
    setBusy("logout");
    try {
      const r = await api.chatgptLogout(profileId);
      setAuthUrl(null);
      await refresh(profileId);
      if (r.revoked) toast.success(t("toast.loggedOut"));
      else toast.warning(t("toast.loggedOutUnconfirmed"));
    } catch (e) {
      toast.error(t("toast.logoutFailed", { msg: (e as Error).message }));
    } finally {
      setBusy(null);
    }
  }

  const connected = status?.status === "connected";
  const noPlan = connected && !status?.plan_usage;
  const reauth = status?.status === "reauth_required";
  const redirectUri = status?.login.redirect_uri;

  let badge = { label: t("status.disconnected"), cls: "border-muted-foreground/40 text-muted-foreground" };
  if (reauth) badge = { label: t("status.reauth"), cls: "border-amber-500/50 text-amber-600 dark:text-amber-400" };
  else if (noPlan) badge = { label: t("status.noPlan"), cls: "border-amber-500/50 text-amber-600 dark:text-amber-400" };
  else if (connected)
    badge = { label: t("status.connected"), cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };

  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <ShieldCheckIcon className="size-4 text-muted-foreground" />
        <span className="font-medium text-sm">{t("title")}</span>
        <Badge variant="outline" className={cn("ml-auto", badge.cls)}>
          {badge.label}
        </Badge>
      </div>
      <p className="text-muted-foreground text-xs leading-relaxed">{t("intro")}</p>

      {connected && status?.email && (
        <p className="text-sm">
          <span className="text-muted-foreground">{t("account")}</span>{" "}
          <span className="font-mono">{status.email}</span>
        </p>
      )}
      {noPlan && <p className="text-amber-600 text-xs dark:text-amber-400">{t("noPlanHint")}</p>}
      {reauth && <p className="text-amber-600 text-xs dark:text-amber-400">{t("reauthHint")}</p>}
      {!connected && !reauth && !pending && status?.login.state === "error" && status.login.error && (
        <p className="text-destructive text-xs">{status.login.error}</p>
      )}
      {!profileId && <p className="text-muted-foreground text-xs">{t("saveFirst")}</p>}

      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" onClick={() => void login(false)} disabled={busy !== null}>
          {busy === "login" ? <Loader2Icon className="animate-spin" /> : <LogInIcon />}
          {connected || reauth ? t("button.relogin") : t("button.login")}
        </Button>
        {noPlan && (
          <Button type="button" size="sm" variant="outline" onClick={() => void login(true)} disabled={busy !== null}>
            {t("button.reconsent")}
          </Button>
        )}
        {(connected || reauth) && (
          <Button type="button" size="sm" variant="outline" onClick={() => void logout()} disabled={busy !== null}>
            {busy === "logout" ? <Loader2Icon className="animate-spin" /> : <LogOutIcon />}
            {t("button.logout")}
          </Button>
        )}
      </div>

      {pending && (
        <div className="grid gap-3 rounded-md border border-dashed p-3">
          <p className="flex items-center gap-2 text-sm">
            <Loader2Icon className="size-4 shrink-0 animate-spin" /> {t("waiting")}
          </p>
          {authUrl && (
            <a
              href={authUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="flex items-center gap-1 text-primary text-xs underline underline-offset-2"
            >
              <ExternalLinkIcon className="size-3.5" /> {t("openLink")}
            </a>
          )}
          <div className="grid gap-2">
            <p className="font-medium text-xs">{t("paste.title")}</p>
            <p className="text-muted-foreground text-xs leading-relaxed">
              {t("paste.desc", { uri: redirectUri ?? "http://127.0.0.1:8787/auth/callback" })}
            </p>
            <div className="flex gap-2">
              <Input
                className="font-mono text-xs"
                placeholder={t("paste.placeholder")}
                value={paste}
                onChange={(e) => setPaste(e.target.value)}
                aria-label={t("paste.aria")}
              />
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="shrink-0"
                disabled={busy !== null || !paste.trim()}
                onClick={() => void submitPaste()}
              >
                {busy === "paste" && <Loader2Icon className="animate-spin" />}
                {t("paste.submit")}
              </Button>
            </div>
          </div>
        </div>
      )}

      <p className="text-muted-foreground text-xs leading-relaxed">{t("limits")}</p>
    </div>
  );
}
