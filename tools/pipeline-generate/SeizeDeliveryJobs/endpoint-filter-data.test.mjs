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
    const wulingCityPattern = "武陵城|武城|陵城";
    const simplifiedTestAreaPattern = "试验园区|试验园|验园区|试验|验园|园区";
    const traditionalTestAreaPattern = "實驗園區|實驗園|驗園區|實驗|驗園|園區";

    assert.ok(wulingCity.raw.includes('// "武陵城"\n    // @i18n-skip'));
    assert.ok(testArea.raw.includes('// "试验园区"\n    // "實驗園區"\n    // @i18n-skip'));

    assert.deepEqual(wulingCity.value, [
        wulingCityPattern,
        "Wuling City",
        "무릉성",
    ]);
    assert.deepEqual(testArea.value, [
        simplifiedTestAreaPattern,
        traditionalTestAreaPattern,
        "Test Area",
        "実験区域",
        "실험 구역",
    ]);
    for (const text of [
        "武陵城",
        "武城",
        "陵城",
    ]) {
        assert.match(text, new RegExp(wulingCityPattern));
    }
    assert.match("//武陵 /武陵城", new RegExp(wulingCityPattern));
    assert.doesNotMatch("武陵", new RegExp(wulingCityPattern));
    assert.doesNotMatch("//武陵 /试验园区", new RegExp(wulingCityPattern));

    for (const text of [
        "试验园区",
        "试验",
        "验园",
        "园区",
        "试验园",
        "验园区",
    ]) {
        assert.match(text, new RegExp(simplifiedTestAreaPattern));
    }
    assert.match("//武陵 /试验园区", new RegExp(simplifiedTestAreaPattern));
    assert.doesNotMatch("實驗園區", new RegExp(simplifiedTestAreaPattern));
    assert.doesNotMatch("武陵", new RegExp(simplifiedTestAreaPattern));
    assert.doesNotMatch("//武陵 /武陵城", new RegExp(simplifiedTestAreaPattern));

    for (const text of [
        "實驗園區",
        "實驗",
        "驗園",
        "園區",
        "實驗園",
        "驗園區",
    ]) {
        assert.match(text, new RegExp(traditionalTestAreaPattern));
    }
    assert.doesNotMatch("试验园区", new RegExp(traditionalTestAreaPattern));
    assert.doesNotMatch("武陵", new RegExp(traditionalTestAreaPattern));
});
