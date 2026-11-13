// @vitest-environment jsdom

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
  WatchTogetherRoomEvent,
  WatchTogetherRoomMember,
  WatchTogetherTransportCommand,
} from "@/lib/watchTogether";
import { WatchTogetherRoomNotices } from "./WatchTogetherRoomNotices";

const asha = { user_id: 8, profile_id: "asha", display_name: "Asha" };
const ravi = { user_id: 7, profile_id: "ravi", display_name: "Ravi" };
const members: WatchTogetherRoomMember[] = [
  { ...ravi, is_host: true, is_self: true, connected: true },
  { ...asha, is_host: false, is_self: false, connected: true },
];

function command(over: Partial<WatchTogetherTransportCommand>): WatchTogetherTransportCommand {
  return {
    command_id: "c1",
    selection_revision: 1,
    action: "pause",
    position_seconds: 0,
    execute_at: new Date(Date.now() + 300).toISOString(),
    issued_at: new Date().toISOString(),
    playback_state: "paused",
    ...over,
  };
}

function renderNotices() {
  let emit: (event: WatchTogetherRoomEvent) => void = () => {};
  render(
    <WatchTogetherRoomNotices
      subscribe={(listener) => {
        emit = listener;
        return () => {};
      }}
      members={members}
      serverTimeOffsetMs={0}
    />,
  );
  return (event: WatchTogetherRoomEvent) => act(() => emit(event));
}

describe("WatchTogetherRoomNotices", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-11-13T12:00:00Z"));
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("names who paused, resumed or jumped, and not the viewer's own actions", () => {
    const emit = renderNotices();
    emit({ kind: "transport", command: command({ issued_by: ravi }) });
    expect(screen.queryByRole("status")).toBeNull();

    emit({ kind: "transport", command: command({ command_id: "c2", issued_by: asha }) });
    emit({
      kind: "transport",
      command: command({
        command_id: "c3",
        action: "seek",
        position_seconds: 3730,
        issued_by: asha,
      }),
    });
    expect(screen.getByText("Asha paused")).toBeTruthy();
    expect(screen.getByText("Asha jumped to 1:02:10")).toBeTruthy();

    act(() => vi.advanceTimersByTime(4_000));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("ignores a replayed command that ran long ago", () => {
    const emit = renderNotices();
    emit({
      kind: "transport",
      command: command({
        issued_by: asha,
        execute_at: new Date(Date.now() - 60_000).toISOString(),
      }),
    });
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("shows a pause and the reaction sent with it as one notice, in either order", () => {
    const emit = renderNotices();
    emit({
      kind: "reaction",
      reaction: {
        reaction_id: "r1",
        preset: "brb",
        from: asha,
        command_id: "c9",
        sent_at: new Date().toISOString(),
      },
    });
    emit({ kind: "transport", command: command({ command_id: "c9", issued_by: asha }) });

    const notices = document.querySelectorAll("[data-room-notice]");
    expect(notices).toHaveLength(1);
    expect(notices[0]?.textContent).toBe("Asha pausedbrb");
  });

  it("confirms the viewer's own reaction as theirs", () => {
    const emit = renderNotices();
    emit({
      kind: "reaction",
      reaction: { reaction_id: "r2", preset: "laugh", from: ravi, sent_at: "" },
    });
    expect(document.querySelector("[data-room-notice]")?.textContent).toBe("You😂");
  });
});
