"""缘缘庄菜单图标识别所需的菜品、饮品候选 ID。"""

from __future__ import annotations

from pathlib import Path
from typing import Any

from ..utils.game_data import read_table_cfg
from .base import Dataset


class Restaurant(Dataset):
    """干员名称、技能与制作时间不参与日常经营决策。"""

    requires_json_data = False

    @classmethod
    def build(cls, table_cfg_dir: Path, json_data_dir: Path) -> dict[str, Any]:
        return {
            "dishes": list(read_table_cfg(table_cfg_dir, "InnDishTable")),
            "drinks": list(read_table_cfg(table_cfg_dir, "InnDrinkTable")),
        }
