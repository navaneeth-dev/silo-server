import { useEffect, useRef, useState } from "react";
import { useT } from "@/i18n";
import {
  reactionLabel,
  type WatchTogetherMemberIdentity,
  type WatchTogetherRoomEvent,
  type WatchTogetherRoomMember,
} from "@/lib/watchTogether";
import { formatTime } from "../utils/formatTime";

/** How long a notice stays up. */
const ROOM_NOTICE_MS = 4_000;
/** Older notices give way to newer ones past this many. */
const MAX_ROOM_NOTICES = 3;
/**
 * A command delivered this long after it ran is a replay (a resent room
 * command), not news.
 */
const STALE_COMMAND_MS = 10_000;

interface RoomNotice {
  /** The command ID, or the reaction ID for a reaction sent alone. */
  key: string;
  /** Bumped on every update, so an expiry timer only removes what it showed. */
  version: number;
  from?: WatchTogetherMemberIdentity;
  action?: "pause" | "play" | "seek";
  positionSeconds?: number;
  reaction?: string;
  rateLimited?: boolean;
}

function sameMember(a: WatchTogetherMemberIdentity, b: WatchTogetherMemberIdentity | undefined) {
  return !!b && a.user_id === b.user_id && a.profile_id === b.profile_id;
}

interface WatchTogetherRoomNoticesProps {
  subscribe: (listener: (event: WatchTogetherRoomEvent) => void) => () => void;
  members?: WatchTogetherRoomMember[];
  serverTimeOffsetMs: number;
}

/**
 * Passing notices about what other members did: "Asha paused", "Asha jumped
 * to 1:02:10", and reactions such as "Asha · brb". The viewer's own play,
 * pause and seek are not announced back to them; their own reactions are, as
 * confirmation that they went out.
 */
export function WatchTogetherRoomNotices({
  subscribe,
  members,
  serverTimeOffsetMs,
}: WatchTogetherRoomNoticesProps) {
  const t = useT();
  const [notices, setNotices] = useState<RoomNotice[]>([]);
  const selfRef = useRef<WatchTogetherRoomMember | undefined>(undefined);
  selfRef.current = members?.find((member) => member.is_self);
  const offsetRef = useRef(serverTimeOffsetMs);
  offsetRef.current = serverTimeOffsetMs;

  useEffect(() => {
    const timers = new Set<number>();
    let version = 0;
    const show = (update: Omit<RoomNotice, "version">) => {
      const shown = ++version;
      setNotices((current) => {
        const existing = current.find((notice) => notice.key === update.key);
        // A pause and the reaction sent with it share the pause's key and
        // show as one notice, whichever arrives first.
        const merged: RoomNotice = { ...existing, ...update, version: shown };
        const rest = current.filter((notice) => notice.key !== update.key);
        return [...rest, merged].slice(-MAX_ROOM_NOTICES);
      });
      const timer = window.setTimeout(() => {
        timers.delete(timer);
        setNotices((current) =>
          current.filter((notice) => notice.key !== update.key || notice.version !== shown),
        );
      }, ROOM_NOTICE_MS);
      timers.add(timer);
    };
    const unsubscribe = subscribe((event) => {
      if (event.kind === "reaction_rate_limited") {
        show({ key: "rate-limited", rateLimited: true });
        return;
      }
      if (event.kind === "reaction") {
        const { reaction } = event;
        show({
          key: reaction.command_id || reaction.reaction_id,
          from: reaction.from,
          reaction: reaction.preset,
        });
        return;
      }
      const { command } = event;
      if (!command.issued_by || sameMember(command.issued_by, selfRef.current)) return;
      const executeAt = Date.parse(command.execute_at);
      if (Date.now() + offsetRef.current - executeAt > STALE_COMMAND_MS) return;
      show({
        key: command.command_id,
        from: command.issued_by,
        action: command.action,
        positionSeconds: command.position_seconds,
      });
    });
    return () => {
      unsubscribe();
      for (const timer of timers) window.clearTimeout(timer);
    };
  }, [subscribe]);

  if (notices.length === 0) return null;
  return (
    <div
      role="status"
      aria-live="polite"
      className="pointer-events-none absolute inset-x-0 top-4 z-40 flex flex-col items-center gap-2 px-4"
    >
      {notices.map((notice) => {
        const label = notice.reaction ? reactionLabel(t, notice.reaction) : null;
        let text: string | null = null;
        if (notice.rateLimited) {
          text = t("web.player.watchTogether.toast.rateLimited");
        } else if (notice.from && notice.action) {
          const name = notice.from.display_name;
          text =
            notice.action === "pause"
              ? t("web.player.watchTogether.toast.paused", { name })
              : notice.action === "play"
                ? t("web.player.watchTogether.toast.resumed", { name })
                : t("web.player.watchTogether.toast.jumped", {
                    name,
                    time: formatTime(notice.positionSeconds ?? 0),
                  });
        } else if (notice.from && label) {
          text = sameMember(notice.from, selfRef.current)
            ? t("web.player.watchTogether.toast.you")
            : notice.from.display_name;
        }
        if (!text) return null;
        return (
          <div
            key={notice.key}
            data-room-notice={notice.key}
            className="hud-card flex max-w-md items-center gap-2 rounded-full px-4 py-2 text-sm font-medium"
          >
            <span className="truncate">{text}</span>
            {label && !notice.rateLimited ? (
              <span className="shrink-0 rounded-full bg-white/15 px-2 py-0.5 text-sm font-semibold">
                {label}
              </span>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}
