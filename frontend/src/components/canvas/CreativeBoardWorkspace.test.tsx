import { act, createEvent, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { CreativeBoardWorkspace } from "./CreativeBoardWorkspace";
import { API } from "@/api";
import { useProjectsStore } from "@/stores/projects-store";
import { useTasksStore } from "@/stores/tasks-store";

const translate = vi.hoisted(() => (key: string) => key);
vi.mock("react-i18next", () => ({
  useTranslation: () => ({ t: translate }),
}));

const enqueueImageEdit = vi.hoisted(() => vi.fn());
const enqueueCanvasImageSplit = vi.hoisted(() => vi.fn());
const enqueueCanvasImageAdvanced = vi.hoisted(() => vi.fn());
vi.mock("@/actions/generation", () => ({
  enqueueImageEdit,
  enqueueCanvasImageSplit,
  enqueueCanvasImageAdvanced,
}));

vi.mock("@/api", () => ({
  API: {
    listCreativeBoards: vi.fn(),
    getCreativeBoard: vi.fn(),
    createCreativeBoard: vi.fn(),
    updateCreativeBoard: vi.fn(),
    getProject: vi.fn(),
    getGlobalAssetUrl: vi.fn((path: string) => path),
    getFileUrl: vi.fn((project: string, path: string) => `/${project}/${path}`),
  },
  getCreativeBoardConflictRevision: vi.fn(),
  isCreativeBoardRevisionConflict: vi.fn(),
}));

const loadCanvasAssets = vi.hoisted(() => vi.fn());
vi.mock("./canvas-assets", () => ({
  loadCanvasAssets,
}));

vi.mock("./CanvasSaveStatus", () => ({
  CanvasSaveStatus: () => null,
}));

vi.mock("./ConflictModal", () => ({
  ConflictModal: () => null,
}));

vi.mock("./CreativeBoardActions", () => ({
  CreativeBoardActions: () => null,
}));

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
};

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((nextResolve) => {
    resolve = nextResolve;
  });
  return { promise, resolve };
}

function makeBoard(id: string, itemId: string, name: string) {
  return {
    id,
    project_id: "demo",
    name,
    viewport: { x: 28, y: 28, zoom: 1 },
    items: [
      {
        id: itemId,
        item_type: "document",
        resource_type: "document",
        resource_id: itemId,
        position: { x: 10, y: 10 },
        size: { width: 100, height: 80 },
      },
    ],
    edges: [],
    revision: 1,
  };
}

describe("CreativeBoardWorkspace", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useProjectsStore.setState(useProjectsStore.getInitialState(), true);
    useProjectsStore.setState({
      currentProjectName: "demo",
      currentProjectData: {
        title: "Demo",
        content_mode: "narration",
        style: "Anime",
        episodes: [],
        characters: {},
        scenes: {},
        props: {},
      },
    });
    vi.mocked(API.listCreativeBoards).mockResolvedValue({
      items: [
        { id: "board-a", name: "Canvas A" },
        { id: "board-b", name: "Canvas B" },
      ],
    } as never);
    vi.mocked(API.getProject).mockResolvedValue({ project: {}, asset_fingerprints: {} } as never);
    vi.mocked(API.getCreativeBoard).mockResolvedValue(makeBoard("board-a", "item-a", "Canvas A") as never);
    loadCanvasAssets.mockResolvedValue({ assets: [], byKind: {}, errors: [] });
    enqueueCanvasImageAdvanced.mockResolvedValue({ taskIds: [], deduped: false });
  });

  it("filters assets by Personal, Agent, and Global categories", async () => {
    loadCanvasAssets.mockResolvedValue({
      assets: [
        { id: "media-1", name: "Personal media", kind: "media", source: "media", resourceType: "media_asset", imagePath: null, reference: { source: "media", kind: "media", id: "media-1", projectName: "demo", requiresImport: false } },
        { id: "character-1", name: "Agent character", kind: "character", source: "project", resourceType: "character", imagePath: null, reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } },
        { id: "scene-1", name: "Agent scene", kind: "scene", source: "project", resourceType: "scene", imagePath: null, reference: { source: "project", kind: "scene", id: "scene-1", projectName: "demo", requiresImport: false } },
        { id: "prop-1", name: "Agent prop", kind: "prop", source: "project", resourceType: "prop", imagePath: null, reference: { source: "project", kind: "prop", id: "prop-1", projectName: "demo", requiresImport: false } },
        { id: "global-1", name: "Global asset", kind: "character", source: "global", resourceType: "character", imagePath: null, reference: { source: "global", kind: "character", id: "global-1", requiresImport: true } },
      ],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByRole("button", { name: "creative_board_assets_label" });
    act(() => screen.getByRole("button", { name: "creative_board_assets_label" }).click());
    expect(await screen.findByText("Personal media")).toBeInTheDocument();
    expect(screen.queryByText("Agent character")).not.toBeInTheDocument();

    act(() => screen.getByRole("tab", { name: "creative_board_asset_category_agent" }).click());
    expect(await screen.findByText("Agent character")).toBeInTheDocument();
    expect(screen.getByText("Agent scene")).toBeInTheDocument();
    expect(screen.getByText("Agent prop")).toBeInTheDocument();
    expect(screen.queryByText("Personal media")).not.toBeInTheDocument();
    expect(screen.queryByText("Global asset")).not.toBeInTheDocument();

    act(() => screen.getByRole("tab", { name: "creative_board_asset_category_global" }).click());
    expect(await screen.findByText("Global asset")).toBeInTheDocument();
    expect(screen.queryByText("Agent character")).not.toBeInTheDocument();
  });

  it("renames a canvas node inline without changing the linked asset", async () => {
    const board = makeBoard("board-a", "item-a", "Canvas A");
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    expect((await screen.findAllByText("item-a")).length).toBeGreaterThan(0);
    const moreButton = screen.getByRole("button", { name: "creative_board_more" });

    act(() => moreButton.click());
    act(() => screen.getByRole("menuitem", { name: "creative_board_rename_item" }).click());
    const input = screen.getByRole("textbox", { name: "creative_board_item_name_input" });
    expect(input).toHaveValue("item-a");

    fireEvent.change(input, { target: { value: "Hero asset" } });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(screen.getAllByText("Hero asset").length).toBeGreaterThan(0);
    expect(screen.getByTestId("creative-board-item-item-a")).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "creative_board_item_name_input" })).not.toBeInTheDocument();
    expect(screen.queryByText("creative_board_inspector")).not.toBeInTheDocument();
    expect(moreButton).toBeInTheDocument();
  });

  it("cancels empty and Escape renames, while blur saves a valid name", async () => {
    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    expect((await screen.findAllByText("item-a")).length).toBeGreaterThan(0);
    const openRename = () => {
      act(() => screen.getByRole("button", { name: "creative_board_more" }).click());
      act(() => screen.getByRole("menuitem", { name: "creative_board_rename_item" }).click());
    };

    openRename();
    let input = screen.getByRole("textbox", { name: "creative_board_item_name_input" });
    fireEvent.change(input, { target: { value: "  " } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getAllByText("item-a").length).toBeGreaterThan(0);

    openRename();
    input = screen.getByRole("textbox", { name: "creative_board_item_name_input" });
    fireEvent.change(input, { target: { value: "Cancelled" } });
    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.getAllByText("item-a").length).toBeGreaterThan(0);

    openRename();
    input = screen.getByRole("textbox", { name: "creative_board_item_name_input" });
    fireEvent.change(input, { target: { value: "Blur saved" } });
    fireEvent.blur(input);
    expect(screen.getAllByText("Blur saved").length).toBeGreaterThan(0);
  });

  it("runs rename, duplicate, and download actions from the element actions menu", async () => {
    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByTestId("creative-board-item-item-a");
    const moreButton = screen.getByRole("button", { name: "creative_board_more" });

    act(() => moreButton.click());
    expect(screen.getByRole("menu")).toBeInTheDocument();
    act(() => fireEvent.pointerDown(document.body));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();

    act(() => moreButton.click());
    act(() => screen.getByRole("menuitem", { name: "creative_board_duplicate_item" }).click());
    expect(screen.getAllByRole("button", { name: "creative_board_more" })).toHaveLength(2);

    const updatedMoreButtons = screen.getAllByRole("button", { name: "creative_board_more" });
    act(() => updatedMoreButtons[0].click());
    expect(screen.getByRole("menuitem", { name: "creative_board_rename_item" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "creative_board_duplicate_item" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "creative_board_download_item" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "creative_board_delete_item" })).not.toBeInTheDocument();
  });


  it("moves a canvas image from both its picture and empty card area", async () => {
    const board = makeBoard("board-a", "item-a", "Canvas A");
    board.items[0] = {
      ...board.items[0],
      item_type: "character",
      resource_type: "character",
      resource_id: "character-1",
    };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{
        id: "project:character:character-1",
        sourceId: "character-1",
        name: "Hero",
        kind: "character",
        source: "project",
        previewUrl: "/hero-avatar.png",
        sidebarPreviewUrl: "/hero-avatar.png",
        canvasPreviewUrl: "/hero-design.png",
        reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false },
      }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-item-a");
    const image = await screen.findByRole("img", { name: "Hero" });
    const canvas = screen.getByTestId("creative-board-canvas");
    expect(image).toHaveAttribute("draggable", "false");
    expect(node).toHaveStyle({ left: "10px", top: "10px" });

    act(() => {
      const pointerDown = createEvent.pointerDown(image, { button: 0, clientX: 100, clientY: 100, pointerId: 7 });
      fireEvent(image, pointerDown);
      expect(pointerDown.defaultPrevented).toBe(true);
      fireEvent.pointerMove(canvas, { clientX: 101, clientY: 101, pointerId: 7 });
    });

    expect(node).toHaveStyle({ left: "10px", top: "10px" });

    act(() => {
      fireEvent.pointerMove(canvas, { clientX: 140, clientY: 130, pointerId: 7 });
    });

    expect(node).toHaveStyle({ left: "50px", top: "40px" });
    expect(node).toHaveClass("cursor-grabbing", "select-none");

    act(() => {
      fireEvent.pointerUp(canvas, { clientX: 140, clientY: 130, pointerId: 7 });
    });

    expect(node).not.toHaveClass("cursor-grabbing", "select-none");

    act(() => {
      fireEvent.pointerUp(canvas, { clientX: 140, clientY: 130, pointerId: 7 });
      fireEvent.pointerDown(node, { button: 0, clientX: 140, clientY: 130, pointerId: 8 });
      fireEvent.pointerMove(canvas, { clientX: 0, clientY: 0, pointerId: 8 });
    });

    expect(node).toHaveStyle({ left: "-90px", top: "-90px" });

    act(() => {
      fireEvent.pointerUp(canvas, { clientX: 0, clientY: 0, pointerId: 8 });
    });
  });

  it("locates an image node and shows the selected-node toolbar", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = {
      ...board.items[0],
      id: "image-node",
      item_type: "character",
      resource_type: "character",
      resource_id: "character-1",
      position: { x: 220, y: 180 },
    };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{
        id: "character-1",
        name: "Hero",
        kind: "character",
        source: "project",
        resourceType: "character",
        canvasPreviewUrl: "/hero.png",
        reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false },
      }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByTestId("creative-board-item-image-node");
    const locateButton = screen.getByRole("button", { name: "creative_board_locate_node" });
    act(() => locateButton.click());

    expect(screen.getByTestId("creative-board-item-image-node")).toHaveClass("border-[#6254d9]");
    const toolbar = screen.getByTestId("creative-board-image-tools");
    expect(toolbar).toBeInTheDocument();
    expect(within(toolbar).getByRole("button", { name: "creative_board_toolbar_portrait" })).toBeInTheDocument();
    expect(within(toolbar).getByRole("button", { name: "creative_board_toolbar_expand" })).toBeInTheDocument();
  });

  it("reports an error when downloading an asset without a URL", async () => {
    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByTestId("creative-board-item-item-a");
    act(() => screen.getByRole("button", { name: "creative_board_more" }).click());
    act(() => screen.getByRole("menuitem", { name: "creative_board_download_item" }).click());

    expect(screen.getByText("creative_board_download_error")).toBeInTheDocument();
  });

  it("shows the image toolbar after a direct pointer click on an image node", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1" };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => {
      fireEvent.pointerDown(node, { button: 0, clientX: 120, clientY: 120, pointerId: 11 });
      fireEvent.pointerUp(node, { button: 0, clientX: 120, clientY: 120, pointerId: 11 });
      fireEvent.click(node);
    });

    expect(screen.getByTestId("creative-board-image-tools")).toBeInTheDocument();
  });

  it("passes dropdown presets to the adjustment panel and reopens it from preview", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1" };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("menuitem", { name: "creative_board_toolbar_portrait" }).click());

    const panel = screen.getByTestId("creative-board-tool-panel");
    expect(panel).toHaveAttribute("data-tool-preset", "quality");
    expect(screen.getByRole("textbox", { name: "creative_board_tool_instruction_label" })).toBeInTheDocument();

    act(() => fireEvent.pointerDown(screen.getByTestId("creative-board-canvas"), { button: 0, pointerId: 22 }));
    expect(screen.queryByRole("textbox", { name: "creative_board_tool_instruction_label" })).not.toBeInTheDocument();
    act(() => screen.getByTestId("creative-board-tool-preview").click());
    expect(screen.getByRole("textbox", { name: "creative_board_tool_instruction_label" })).toBeInTheDocument();
  });

  it("submits an HD preset through the advanced canvas-image action", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1" };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("button", { name: "creative_board_toolbar_hd" }).click());
    act(() => within(toolbar).getByRole("menuitem", { name: "creative_board_toolbar_hd" }).click());
    const editor = await screen.findByTestId("canvas-image-editor-config");
    const instruction = within(editor).getByRole("textbox", { name: "creative_board_tool_instruction_label" });
    fireEvent.change(instruction, { target: { value: "保留构图并增强细节" } });
    act(() => screen.getByTestId("canvas-image-editor-run").click());

    await waitFor(() => expect(enqueueCanvasImageAdvanced).toHaveBeenCalledWith("demo", {
      operation: "canvas_image_hd",
      sourceKind: "project",
      resourceType: "character",
      resourceId: "character-1",
      instruction: "保留构图并增强细节",
      count: 1,
      multiplier: 2,
    }));
  });

  it.each([
    ["creative_board_toolbar_panorama", "canvas_image_panorama"],
    ["creative_board_toolbar_angles", "canvas_image_angles"],
    ["creative_board_toolbar_layers", "canvas_image_layers"],
  ] as const)("submits %s as an independent advanced canvas task", async (label, operation) => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1" };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("button", { name: label }).click());
    const instruction = screen.getByRole("textbox", { name: "creative_board_tool_instruction_label" });
    fireEvent.change(instruction, { target: { value: "preserve the subject" } });
    act(() => screen.getByRole("button", { name: "creative_board_tool_apply_adjustment" }).click());

    await waitFor(() => expect(enqueueCanvasImageAdvanced).toHaveBeenCalledWith("demo", {
      operation,
      sourceKind: "project",
      resourceType: "character",
      resourceId: "character-1",
      instruction: "preserve the subject",
    }));
  });

  it("submits custom grid configuration for a project image", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1" };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("menuitem", { name: "creative_board_tool_grid_pending" }).click());

    fireEvent.change(screen.getByTestId("creative-board-grid-rows"), { target: { value: "4" } });
    fireEvent.change(screen.getByTestId("creative-board-grid-cols"), { target: { value: "2" } });
    fireEvent.click(screen.getByTestId("creative-board-grid-split-lines"));
    act(() => screen.getByRole("button", { name: "creative_board_tool_apply_adjustment" }).click());

    await waitFor(() => expect(enqueueCanvasImageSplit).toHaveBeenCalledWith("demo", {
      sourceKind: "project",
      resourceType: "character",
      resourceId: "character-1",
      rows: 4,
      cols: 2,
      includeSplitLines: false,
    }));
  });

  it("submits personal media as an independent canvas split source", async () => {
    const board = makeBoard("board-a", "media-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "media-node", item_type: "media", resource_type: "media_asset", resource_id: "media-1" };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "media-1", name: "Reference image", kind: "media", source: "media", resourceType: "media_asset", canvasPreviewUrl: "/media.png", reference: { source: "media", kind: "media", id: "media-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-media-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("menuitem", { name: "creative_board_toolbar_split3x3" }).click());
    expect(screen.getByTestId("creative-board-grid-rows")).toHaveValue(3);
    expect(screen.getByTestId("creative-board-grid-cols")).toHaveValue(3);
    act(() => screen.getByRole("button", { name: "creative_board_tool_apply_adjustment" }).click());

    await waitFor(() => expect(enqueueCanvasImageSplit).toHaveBeenCalledWith("demo", {
      sourceKind: "media",
      mediaAssetId: "media-1",
      rows: 3,
      cols: 3,
      includeSplitLines: true,
    }));
  });

  it("writes successful split cells back as one grouped set while preserving the source", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1", size: { width: 240, height: 240 } };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });
    enqueueCanvasImageSplit.mockResolvedValue({ taskIds: ["split-task"], deduped: false });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("menuitem", { name: "creative_board_tool_grid_pending" }).click());
    act(() => screen.getByRole("button", { name: "creative_board_tool_apply_adjustment" }).click());
    await waitFor(() => expect(enqueueCanvasImageSplit).toHaveBeenCalled());

    const task = {
      task_id: "split-task",
      project_name: "demo",
      task_type: "canvas_image_split",
      media_type: "image",
      resource_id: "character-1",
      resource_type: null,
      script_file: null,
      payload: {},
      status: "succeeded",
      result: {
        operation: "canvas_image_split",
        rows: 2,
        cols: 2,
        include_split_lines: true,
        cells: [
          { row: 0, col: 0, index: 0, media_asset_id: "cell-0" },
          { row: 0, col: 1, index: 1, media_asset_id: "cell-1" },
          { row: 1, col: 0, index: 2, media_asset_id: "cell-2" },
          { row: 1, col: 1, index: 3, media_asset_id: "cell-3" },
        ],
      },
      error_message: null,
      cancelled_by: null,
      provider_id: null,
      provider_job_id: null,
      source: "webui",
      queued_at: "2026-01-01T00:00:00Z",
      started_at: "2026-01-01T00:00:01Z",
      finished_at: "2026-01-01T00:00:02Z",
      updated_at: "2026-01-01T00:00:02Z",
    } as never;
    act(() => useTasksStore.setState({ tasks: [task] }));

    await waitFor(() => expect(screen.getAllByTestId(/^creative-board-item-/)).toHaveLength(5));
    expect(screen.getByTestId("creative-board-item-image-node")).toBeInTheDocument();
    act(() => useTasksStore.setState({ tasks: [task] }));
    expect(screen.getAllByTestId(/^creative-board-item-/)).toHaveLength(5);
  });

  it("writes advanced outputs back as grouped new nodes while preserving the source", async () => {
    const board = makeBoard("board-a", "image-node", "Canvas A");
    board.items[0] = { ...board.items[0], id: "image-node", item_type: "character", resource_type: "character", resource_id: "character-1", size: { width: 240, height: 240 } };
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [{ id: "character-1", name: "Hero", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/hero.png", reference: { source: "project", kind: "character", id: "character-1", projectName: "demo", requiresImport: false } }],
      byKind: {},
      errors: [],
    });
    enqueueCanvasImageAdvanced.mockResolvedValue({ taskIds: ["advanced-task"], deduped: false });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    const node = await screen.findByTestId("creative-board-item-image-node");
    act(() => fireEvent.click(node));
    const toolbar = screen.getByTestId("creative-board-image-tools");
    act(() => within(toolbar).getByRole("button", { name: "creative_board_toolbar_panorama" }).click());
    fireEvent.change(screen.getByRole("textbox", { name: "creative_board_tool_instruction_label" }), { target: { value: "preserve the subject" } });
    act(() => screen.getByRole("button", { name: "creative_board_tool_apply_adjustment" }).click());
    await waitFor(() => expect(enqueueCanvasImageAdvanced).toHaveBeenCalled());

    const task = {
      task_id: "advanced-task",
      project_name: "demo",
      task_type: "canvas_image_panorama",
      media_type: "image",
      resource_id: "character-1",
      resource_type: null,
      script_file: null,
      payload: {},
      status: "succeeded",
      result: {
        operation: "canvas_image_panorama",
        outputs: [
          { index: 0, label: "panorama-left", media_asset_id: "output-1", width: 1600, height: 800 },
          { index: 1, label: "panorama-right", media_asset_id: "output-2", width: 1600, height: 800 },
        ],
      },
      error_message: null,
      cancelled_by: null,
      provider_id: null,
      provider_job_id: null,
      source: "webui",
      queued_at: "2026-01-01T00:00:00Z",
      started_at: "2026-01-01T00:00:01Z",
      finished_at: "2026-01-01T00:00:02Z",
      updated_at: "2026-01-01T00:00:02Z",
    } as never;
    act(() => useTasksStore.setState({ tasks: [task] }));

    await waitFor(() => expect(screen.getAllByTestId(/^creative-board-item-/)).toHaveLength(3));
    expect(screen.getByTestId("creative-board-item-image-node")).toBeInTheDocument();
    expect(screen.getAllByTestId(/^creative-board-item-/)[1]).toHaveStyle({ width: "240px", height: "120px" });
  });

  it("shows image tools for Personal, Agent, and Global image assets, but not document nodes", async () => {
    const board = makeBoard("board-a", "personal-image", "Canvas A");
    board.items = [
      { ...board.items[0], id: "personal-image", item_type: "media", resource_type: "media_asset", resource_id: "personal-image", position: { x: 10, y: 10 } },
      { ...board.items[0], id: "agent-image", item_type: "character", resource_type: "character", resource_id: "agent-image", position: { x: 150, y: 10 } },
      { ...board.items[0], id: "global-image", item_type: "scene", resource_type: "scene", resource_id: "global-image", position: { x: 290, y: 10 } },
      { ...board.items[0], id: "document-node", item_type: "document", resource_type: "document", resource_id: "document-node", position: { x: 430, y: 10 } },
    ];
    vi.mocked(API.getCreativeBoard).mockResolvedValue(board as never);
    loadCanvasAssets.mockResolvedValue({
      assets: [
        { id: "personal-image", name: "Personal image", kind: "media", source: "media", resourceType: "media_asset", canvasPreviewUrl: "/personal.png", reference: { source: "media", kind: "media", id: "personal-image", projectName: "demo", requiresImport: false } },
        { id: "agent-image", name: "Agent image", kind: "character", source: "project", resourceType: "character", canvasPreviewUrl: "/agent.png", reference: { source: "project", kind: "character", id: "agent-image", projectName: "demo", requiresImport: false } },
        { id: "global-image", name: "Global image", kind: "scene", source: "global", resourceType: "scene", canvasPreviewUrl: "/global.png", reference: { source: "global", kind: "scene", id: "global-image", requiresImport: true } },
      ],
      byKind: {},
      errors: [],
    });

    render(
      <Router hook={memoryLocation({ path: "/creative-board" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByTestId("creative-board-item-personal-image");
    for (const id of ["personal-image", "agent-image", "global-image"]) {
      act(() => fireEvent.click(screen.getByTestId(`creative-board-item-${id}`)));
      expect(screen.getByTestId("creative-board-image-tools")).toBeInTheDocument();
    }

    act(() => fireEvent.click(screen.getByTestId("creative-board-item-document-node")));
    expect(screen.queryByTestId("creative-board-image-tools")).not.toBeInTheDocument();
    expect(screen.queryByText("creative_board_inspector")).not.toBeInTheDocument();
  });

  it("switches boards without duplicating the existing query string", async () => {
    const location = memoryLocation({ path: "/creative-board?project=demo&board=board-a&episode=2", record: true });

    render(
      <Router hook={location.hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByText("Canvas A");
    act(() => screen.getByRole("button", { name: "creative_board_switch" }).click());
    act(() => screen.getByRole("menuitem", { name: /Canvas B/ }).click());

    expect(location.history.at(-1)).toBe("/creative-board?project=demo&board=board-b&episode=2");
  });


  it("keeps the newly selected board when an older board request resolves later", async () => {
    const boardA = deferred<ReturnType<typeof makeBoard>>();
    const boardB = deferred<ReturnType<typeof makeBoard>>();
    vi.mocked(API.getCreativeBoard).mockImplementation((boardId: string) =>
      (boardId === "board-a" ? boardA.promise : boardB.promise) as never,
    );
    const location = memoryLocation({ path: "/creative-board?project=demo&board=board-a" });

    render(
      <Router hook={location.hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await waitFor(() => expect(API.getCreativeBoard).toHaveBeenCalledWith("board-a"));

    act(() => {
      location.navigate("/creative-board?project=demo&board=board-b");
    });
    await waitFor(() => expect(API.getCreativeBoard).toHaveBeenCalledWith("board-b"));

    await act(async () => {
      boardB.resolve(makeBoard("board-b", "item-b", "Canvas B"));
    });
    expect((await screen.findAllByText("item-b")).length).toBeGreaterThan(0);

    await act(async () => {
      boardA.resolve(makeBoard("board-a", "item-a", "Canvas A"));
    });
    await waitFor(() => expect(screen.getAllByText("item-b").length).toBeGreaterThan(0));
    expect(screen.queryByText("item-a")).not.toBeInTheDocument();
  });

  it("falls back to an existing board when the URL board no longer exists", async () => {
    vi.mocked(API.getCreativeBoard)
      .mockRejectedValueOnce({ status: 404, message: "creative_board_not_found" } as never);

    render(
      <Router hook={memoryLocation({ path: "/creative-board?project=demo&board=board-gone" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    expect((await screen.findAllByText("item-a")).length).toBeGreaterThan(0);
  });

  it("renames a newly created board with expected_revision and applies the returned revision", async () => {
    vi.mocked(API.getCreativeBoard).mockImplementation((boardId: string) =>
      (boardId === "board-new"
        ? makeBoard("board-new", "item-new", "New Canvas")
        : makeBoard("board-a", "item-a", "Canvas A")) as never);
    vi.mocked(API.createCreativeBoard).mockResolvedValue(makeBoard("board-new", "item-new", "New Canvas") as never);
    vi.mocked(API.updateCreativeBoard).mockResolvedValue({ revision: 2 } as never);

    render(
      <Router hook={memoryLocation({ path: "/creative-board?project=demo&board=board-a" }).hook}>
        <Route path="/creative-board">
          <CreativeBoardWorkspace projectName="demo" />
        </Route>
      </Router>,
    );

    await screen.findByText("Canvas A");
    act(() => screen.getByRole("button", { name: "creative_board_switch" }).click());
    act(() => screen.getByRole("button", { name: "creative_board_add" }).click());

    const input = await screen.findByLabelText("creative_board_name_input");
    expect(input).toHaveValue("New Canvas");
    fireEvent.change(input, { target: { value: "Renamed canvas" } });
    fireEvent.blur(input);

    await waitFor(() => expect(API.updateCreativeBoard).toHaveBeenCalledWith("board-new", {
      name: "Renamed canvas",
      expected_revision: 1,
    }));
  });
});
