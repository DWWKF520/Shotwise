import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { API } from "@/api";
import { useProjectsStore } from "@/stores/projects-store";
import { AssetSidebar } from "@/components/layout/AssetSidebar";

vi.mock("@/api", () => ({
  API: {
    listFiles: vi.fn(),
    getCostEstimate: vi.fn(),
  },
}));

function renderSidebar(path = "/app/projects/demo/characters") {
  const location = memoryLocation({ path, record: true });
  const view = render(
    <Router hook={location.hook}>
      <Route path="/app/projects/:projectName" nest>
        <AssetSidebar />
      </Route>
    </Router>,
  );
  return { ...view, location };
}

/**
 * 拖拽手柄辅助：模拟一次完整的按下-移动-抬起序列。
 * mousemove 必须带 buttons: 1（主键按住），组件靠它区分移动与收尾。
 */
function dragHandle(
  handle: HTMLElement,
  from: { clientX: number },
  to: { clientX: number },
) {
  fireEvent.mouseDown(handle, { button: 0, ...from });
  fireEvent.mouseMove(window, { buttons: 1, ...to });
  fireEvent.mouseUp(window);
}

describe("AssetSidebar", () => {
  beforeEach(() => {
    useProjectsStore.setState(useProjectsStore.getInitialState(), true);
    useProjectsStore.setState({
      currentProjectName: "demo",
      currentProjectData: {
        title: "Demo",
        content_mode: "narration",
        style: "Anime",
        episodes: [{ episode: 1, title: "第一集", script_file: "scripts/episode_1.json" }],
        characters: {},
        scenes: {},
        props: {},
      },
    });
    vi.mocked(API.listFiles).mockResolvedValue({ files: { source: [] } } as never);
    vi.mocked(API.getCostEstimate).mockResolvedValue({ episodes: [] } as never);
    window.localStorage.clear();
  });

  it("切换到创作画布时保留项目路由并继续显示侧栏", () => {
    const openSpy = vi.spyOn(window, "open").mockImplementation(() => null);
    const { location } = renderSidebar();

    fireEvent.click(screen.getByRole("button", { name: "创作画布" }));

    expect(location.history).toEqual([
      "/app/projects/demo/characters",
      "/app/projects/demo/creative-board",
    ]);
    expect(openSpy).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "创作画布" })).toBeInTheDocument();
  });

  it("拖宽侧栏生效并持久化，折叠态不覆盖上次宽度", () => {
    renderSidebar();
    const aside = screen.getByRole("complementary");
    const handle = screen.getByRole("separator", { name: "拖动调整侧栏宽度" });

    dragHandle(handle, { clientX: 256 }, { clientX: 320 });

    expect(aside).toHaveStyle({ width: "320px" });
    expect(window.localStorage.getItem("shotwise:sidebarWidth")).toBe("320");

    // 折叠（点击既有按钮）不把 64 写进持久化槽位
    fireEvent.click(screen.getByRole("button", { name: "折叠" }));
    expect(aside).toHaveStyle({ width: "64px" });
    expect(window.localStorage.getItem("shotwise:sidebarWidth")).toBe("320");

    // 展开后回到拖拽出的宽度
    fireEvent.click(screen.getByRole("button", { name: "展开" }));
    expect(aside).toHaveStyle({ width: "320px" });
  });

  it("拖到低于阈值时折叠，折叠态向外拖越过阈值展开", () => {
    renderSidebar();
    const handle = screen.getByRole("separator", { name: "拖动调整侧栏宽度" });

    // 256 → 180：低于 200 阈值，拖动中即折叠
    dragHandle(handle, { clientX: 256 }, { clientX: 180 });

    expect(screen.getByRole("button", { name: "展开" })).toBeInTheDocument();

    // 折叠态以 64 为基点，拖到 200 时 live 宽度 264 ≥ 200 展开
    dragHandle(handle, { clientX: 0 }, { clientX: 200 });

    expect(screen.getByRole("button", { name: "折叠" })).toBeInTheDocument();
  });

  it("拖动宽度不超过上限", () => {
    renderSidebar();
    const aside = screen.getByRole("complementary");
    const handle = screen.getByRole("separator", { name: "拖动调整侧栏宽度" });

    dragHandle(handle, { clientX: 256 }, { clientX: 9999 });

    expect(aside).toHaveStyle({ width: "420px" });
  });
});
