import assert from "node:assert/strict";
import test from "node:test";

import {candidatesRows} from "./endpoint-filter-data.mjs";

test("builds language-specific Wuling area OCR patterns", () => {
    const expectedByArea = Object.fromEntries(
        candidatesRows.map((row) => [
            row.AreaId,
            row.Expected,
        ]),
    );
    const wulingCity = expectedByArea.WulingCity;
    const testArea = expectedByArea.TestArea;
    const simplifiedTestArea = testArea.value.find((value) => value === "试?验园区");
    const traditionalTestArea = testArea.value.find((value) => value === "實?驗園區");

    assert.ok(wulingCity.raw.includes('// "武陵城"\n    // @i18n-skip'));
    assert.ok(testArea.raw.includes('// "试验园区"\n    // "實驗園區"\n    // @i18n-skip'));

    assert.deepEqual(wulingCity.value, [
        "武?陵城",
        "Wuling City",
        "무릉성",
    ]);
    assert.match("武陵城", new RegExp(wulingCity.value[0]));
    assert.match("陵城", new RegExp(wulingCity.value[0]));

    assert.match("试验园区", new RegExp(simplifiedTestArea));
    assert.match("验园区", new RegExp(simplifiedTestArea));
    assert.doesNotMatch("實驗園區", new RegExp(simplifiedTestArea));

    assert.match("實驗園區", new RegExp(traditionalTestArea));
    assert.match("驗園區", new RegExp(traditionalTestArea));
    assert.doesNotMatch("试验园区", new RegExp(traditionalTestArea));
});
