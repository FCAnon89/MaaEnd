import {readFileSync} from "node:fs";
import {resolve} from "node:path";

import {dataDir} from "../utils/paths.mjs";

function loadItemIds() {
    const sourcePath = resolve(dataDir, "usable_items.json");
    let data;
    try {
        data = JSON.parse(readFileSync(sourcePath, "utf8"));
    } catch (cause) {
        throw new Error(`[UseConsumable] 无法读取 ${sourcePath}，请先运行 uv run zmdmap generate --only usable_items`, {
            cause,
        });
    }
    const items = data?.items;
    if (!items || typeof items !== "object" || Array.isArray(items)) {
        throw new Error(`[UseConsumable] ${sourcePath} 的 items 必须是对象`);
    }
    const itemIds = Object.keys(items).sort();
    if (itemIds.length === 0) {
        throw new Error(`[UseConsumable] ${sourcePath} 中没有可供选择的主动持续增幅道具`);
    }
    return itemIds;
}

const itemIds = loadItemIds();

export default [
    {
        name: "UseConsumable",
        label: "$task.UseConsumable.label",
        entry: "UseConsumableFailureCollectorReset",
        description: "$task.UseConsumable.description",
        group: ["backpack"],
        controller: [
            "ADB",
            "CloudADB",
            "PlayCover",
            "Linux-Gamescope",
            "Linux-ScreenCast",
            "Linux-Wlroots",
            "Win32-Front",
        ],
        slots: [
            1,
            2,
            3,
            4,
        ],
        defaultSlotCase: "Yes",
        itemIds,
        defaultItemId: itemIds[0],
        regions: [
            {id: "ValleyIV", scene: "SceneEnterWorldValleyIVTheHubHubBase"},
            {id: "Wuling", scene: "SceneEnterWorldWulingWulingCityFangxingAvenue"},
        ],
        defaultRegion: "ValleyIV",
    },
];
