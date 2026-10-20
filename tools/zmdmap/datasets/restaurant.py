"""缘缘庄日常经营所需的菜品、饮品、等级与干员烹饪技能数据。"""

from __future__ import annotations

from pathlib import Path
from typing import Any

from ..utils.game_data import as_text, read_table_cfg, ref_texts
from .base import Dataset, DatasetError


# 日常经营界面的流行趋势说明：每匹配一条热门信息，额外获得 10% 赏钱。
POPULARITY_TIP_RATE = 0.1


def _single(values: Any, label: str) -> Any:
    if not isinstance(values, list) or not values:
        raise DatasetError(f"{label} 缺少数值")
    return values[0]


def _menu_item(
    table_cfg_dir: Path,
    item_id: str,
    row: Any,
    items: Any,
    label: str,
) -> dict[str, Any]:
    item = items.get(item_id)
    if item is None:
        raise DatasetError(f"{label} {item_id} 不在 ItemTable 中")
    names = ref_texts(table_cfg_dir, item.get("name"))
    if any(not names.get(language) for language in ("CN", "TC", "EN", "JP", "KR")):
        raise DatasetError(f"{label} {item_id} 缺少完整的五语名称")
    return {
        "id": item_id,
        "names": names,
        "icon_id": as_text(item.get("iconId")),
        "rarity": item.get("rarity"),
        "price": row.get("price"),
        "unlock_level": row.get("unlockLevel"),
        "affixes": row.get("affix") or [],
    }


class Restaurant(Dataset):
    """日常经营计算与图标识别所需的精简游戏数据。"""

    requires_json_data = False

    @classmethod
    def build(cls, table_cfg_dir: Path, json_data_dir: Path) -> dict[str, Any]:
        del json_data_dir
        dishes = read_table_cfg(table_cfg_dir, "InnDishTable")
        drinks = read_table_cfg(table_cfg_dir, "InnDrinkTable")
        levels = read_table_cfg(table_cfg_dir, "InnLevelTable")
        skills = read_table_cfg(table_cfg_dir, "InnSkillTable")
        character_skills = read_table_cfg(table_cfg_dir, "InnCharSkillTable")
        characters = read_table_cfg(table_cfg_dir, "CharacterTable")
        affixes = read_table_cfg(table_cfg_dir, "InnAffixTable")
        items = read_table_cfg(table_cfg_dir, "ItemTable")

        output_dishes: list[dict[str, Any]] = []
        for item_id, row in dishes.items():
            entry = _menu_item(table_cfg_dir, item_id, row, items, "菜品")
            entry.update(
                {
                    "category": as_text(row.get("category")),
                    "time_seconds": row.get("timeCost"),
                    "unlock_topic_id": as_text(row.get("unlockTopicId")),
                    "unlock_topic_stars": row.get("unlockTopicStarNum", 0),
                }
            )
            if not entry["category"] or not isinstance(entry["time_seconds"], int):
                raise DatasetError(f"菜品 {item_id} 缺少类别或制作时长")
            output_dishes.append(entry)

        output_drinks = [
            _menu_item(table_cfg_dir, item_id, row, items, "饮品")
            for item_id, row in drinks.items()
        ]

        output_levels: list[dict[str, Any]] = []
        for key, row in levels.items():
            dish_range = row.get("menuDishNumRange")
            drink_range = row.get("menuDrinkNumRange")
            if not isinstance(dish_range, list) or not dish_range or not isinstance(drink_range, list) or not drink_range:
                raise DatasetError(f"经营等级 {key} 缺少菜单数量要求")
            output_levels.append(
                {
                    "level": int(row.get("innManagementLevel", key)),
                    "minimum_dishes": int(dish_range[0]),
                    "maximum_dishes": int(dish_range[-1]),
                    "minimum_categories": int(row.get("menuDishTypeNumMin", 0)),
                    "usable_categories": row.get("usableDishType") or [],
                    "minimum_drinks": int(drink_range[0]),
                    "maximum_drinks": int(drink_range[-1]),
                }
            )
        output_levels.sort(key=lambda row: row["level"])

        output_skills: list[dict[str, Any]] = []
        for skill_id, row in skills.items():
            skill_type = row.get("skillType")
            if skill_type not in (0, 1):
                continue
            parameters = row.get("parameters") or []
            if len(parameters) < 3:
                raise DatasetError(f"干员技能 {skill_id} 缺少参数")
            effect_value = as_text(_single(parameters[1].get("valueStringList"), f"技能 {skill_id} 效果"))
            speed_bonus = float(_single(parameters[2].get("valueFloatList"), f"技能 {skill_id} 烹饪速度"))
            output_skills.append(
                {
                    "id": skill_id,
                    "effect": "category_speed" if skill_type == 0 else "affix_speed",
                    "category": effect_value if skill_type == 0 else "",
                    "affix": effect_value if skill_type == 1 else "",
                    "speed_bonus": speed_bonus,
                    "unlock_level": int(row.get("unlockLevel", 0)),
                }
            )

        output_characters: list[dict[str, Any]] = []
        for character_id, skill_row in character_skills.items():
            character = characters.get(character_id)
            if character is None:
                raise DatasetError(f"干员技能映射 {character_id} 不在 CharacterTable 中")
            names = ref_texts(table_cfg_dir, character.get("name"))
            if any(not names.get(language) for language in ("CN", "TC", "EN", "JP", "KR")):
                raise DatasetError(f"干员 {character_id} 缺少完整的五语名称")
            output_characters.append(
                {
                    "id": character_id,
                    "names": names,
                    "inn_skill_ids": skill_row.get("list") or [],
                }
            )

        affix_names: dict[str, str] = {}
        for affix_id, row in affixes.items():
            name = ref_texts(table_cfg_dir, row.get("name")).get("CN", "")
            if name:
                affix_names[affix_id] = name

        return {
            "levels": output_levels,
            "dishes": output_dishes,
            "drinks": output_drinks,
            "characters": output_characters,
            "skills": output_skills,
            "affix_names": affix_names,
            "popularity_tip_rate": POPULARITY_TIP_RATE,
        }
