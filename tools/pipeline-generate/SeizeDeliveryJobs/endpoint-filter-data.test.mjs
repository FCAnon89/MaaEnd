import assert from "node:assert/strict";
import test from "node:test";

import {dispatcherRows} from "./endpoint-filter-data.mjs";

test("waits for a recognized area before treating an endpoint as unmatched", () => {
    assert.equal(dispatcherRows.length, 1);
    assert.equal(dispatcherRows[0].NextList.includes("SeizeDeliveryJobsEndpointNotMatched"), false);
});
