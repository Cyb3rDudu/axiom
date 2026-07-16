"""
Outline validation and correction utilities with reflection loop support.
Handles deduplication, depth checking, and iterative refinement.
"""

import logging
from typing import List, Dict, Set, Tuple, Optional, Any
from collections import defaultdict
import difflib

from ai_researcher.agentic_layer.schemas.planning import ReportSection
from ai_researcher.dynamic_config import get_max_total_depth

logger = logging.getLogger(__name__)


def _normalize_title_for_match(title: str) -> str:
    """Normalize a section title for fuzzy matching.

    Order-independent: strips leading heading markers (``##``), leading numeric
    labels (``1.`` / ``2.1`` / ``2.1.``) and trailing punctuation, then
    lower-cases and collapses whitespace. Applied iteratively until stable so
    that all of these compare equal:

    - ``"1. Einleitung"``
    - ``"## 2.1 NexMach als Unternehmung"``
    - ``"Einleitung"``
    - ``"einleitung."``

    The previous implementation ran the numeric-strip once *before* stripping
    heading markers, so ``## 2.1 Foo`` survived as `` 2.1 foo``. The loop here
    fixes that (review finding 2).
    """
    import re
    t = (title or "").strip()
    # Iteratively strip leading heading markers and numeric labels until stable.
    # Bounded loop guards against pathological inputs.
    for _ in range(5):
        prev = t
        t = t.lstrip("#").lstrip()
        m = re.match(r"\d+(?:\.\d+)*[.)]?\s*", t)
        if m:
            t = t[m.end():]
        if t == prev:
            break
    t = t.lower().strip()
    t = t.strip("#:.-_ ").strip()
    t = re.sub(r"\s+", " ", t).strip()
    return t


class OutlineValidator:
    """Validates and corrects outline structure with configurable rules."""
    
    def __init__(self, mission_id: Optional[str] = None, controller: Optional[Any] = None):
        """
        Initialize the validator with mission-specific settings.
        
        Args:
            mission_id: Optional mission ID for fetching mission-specific settings
            controller: Optional controller instance to get max_depth from
        """
        self.mission_id = mission_id
        
        # Get max depth from controller if available, otherwise use dynamic config
        if controller and hasattr(controller, 'max_total_depth'):
            self.max_depth = controller.max_total_depth
        else:
            self.max_depth = get_max_total_depth(mission_id) if mission_id else 2
            
        self.validation_issues = []
        self.corrections_made = []
        
    def validate_and_correct(
        self, 
        outline: List[ReportSection],
        auto_correct: bool = True
    ) -> Tuple[List[ReportSection], Dict[str, Any]]:
        """
        Validates and optionally corrects the outline structure.
        
        Args:
            outline: The outline to validate
            auto_correct: Whether to automatically apply corrections
            
        Returns:
            Tuple of (corrected_outline, validation_report)
        """
        self.validation_issues = []
        self.corrections_made = []
        
        # Create a deep copy to avoid modifying the original
        import copy
        working_outline = copy.deepcopy(outline)
        
        # 1. Check and fix depth issues
        working_outline = self._check_depth(working_outline, auto_correct)
        
        # 2. Check and fix duplications
        working_outline = self._check_duplicates(working_outline, auto_correct)
        
        # 3. Check for empty sections
        working_outline = self._check_empty_sections(working_outline, auto_correct)
        
        # 4. Validate section IDs are unique
        working_outline = self._ensure_unique_ids(working_outline, auto_correct)
        
        # 5. Check and fix research strategies
        working_outline = self._validate_and_correct_strategies(working_outline, auto_correct)
        
        # 6. Remove References/Bibliography sections (they're auto-generated)
        working_outline = self._remove_references_sections(working_outline, auto_correct)
        
        # Generate validation report
        report = {
            "valid": len(self.validation_issues) == 0,
            "issues": self.validation_issues,
            "corrections": self.corrections_made,
            "max_depth_setting": self.max_depth,
            "actual_max_depth": self._calculate_max_depth(working_outline),
            "total_sections": self._count_sections(working_outline),
            "duplicate_sections_found": len([i for i in self.validation_issues if i["type"] == "duplicate"]),
            "has_research_based": self._has_research_based_section(working_outline)
        }
        
        return working_outline, report

    # ------------------------------------------------------------------
    # Required-outline enforcement (Finding 1, deterministic merge)
    # ------------------------------------------------------------------

    def enforce_required_outline(
        self,
        outline: List[ReportSection],
        required_outline: List[Dict[str, Any]],
    ) -> Tuple[List[ReportSection], Dict[str, Any]]:
        """Deterministically ensure every required briefing section is present.

        Unlike the LLM-driven reflection loop, this is a pure post-LLM check: it
        matches the generated ``outline`` against the user's required
        ``structured_outline`` (number / title / level records parsed from the
        Gliederung) and INSERTS any missing required section — including nested
        subsections (level >= 2) — at the correct position, preserving the
        user's order and hierarchy. Existing sections are left untouched.

        The production failure this guards against: the LLM dropped a required
        level-2 subsection (``3.2 Branchen- und Wettbewerbsumwelt``). The first
        iteration of this method only handled top-level (level 1) sections and
        silently ignored nested required subsections (review finding 1). This
        version builds the required tree and recurses into matched parents.

        Args:
            outline: the generated/corrected outline (a list of top-level
                ``ReportSection`` possibly with subsections).
            required_outline: flat list of ``{"number","title","level"}`` dicts
                from mission metadata ``structured_outline``.

        Returns:
            (updated_outline, report) where report lists matched / inserted /
            missing_required (across all levels).
        """
        report: Dict[str, Any] = {
            "required_count": len(required_outline),
            "matched": [],
            "inserted": [],
            "missing_required": [],
        }
        if not required_outline:
            return outline, report

        required_tree = self._build_required_tree(required_outline)

        # Flatten ALL generated sections into a single global pool. This is the
        # key fix for review finding 1 (flat subsections): required subsections
        # may exist as flat top-level siblings (e.g. the planner emitted 3,
        # 3.1, 3.3 all at top-level) instead of nested under their parent. A
        # global pool lets a required subsection be claimed regardless of where
        # it currently sits, and then be moved into its matched parent.
        pool: List[Dict[str, Any]] = []
        self._flatten_generated(outline, pool, original_parent=None)

        result = self._rebuild_from_required(required_tree, pool, report)

        # Re-attach generated sections that were not claimed by any required
        # node (LLM extras). Preserve their original nesting: an extra that was
        # nested under a (now-matched) parent stays nested under it; a top-level
        # extra stays at the top level.
        self._reattach_extras(result, pool)

        if report["inserted"]:
            logger.warning(
                "OutlineValidator: enforce_required_outline inserted %d missing "
                "section(s): %s",
                len(report["inserted"]), report["inserted"],
            )
        else:
            logger.info(
                "OutlineValidator: required-outline check OK — all %d required "
                "section(s) present.",
                report.get("required_count", 0),
            )
        return result, report

    # ----- helpers for enforce_required_outline -----

    @staticmethod
    def _number_key(number: str) -> Tuple[int, ...]:
        """Parse a section number like '3.2' into a sortable tuple (3, 2).

        Non-numeric / empty numbers sort as ``(0,)`` so parents sort before
        children and malformed entries don't crash the sort.
        """
        if not number:
            return (0,)
        parts = []
        for p in str(number).split("."):
            p = p.strip()
            if p.isdigit():
                parts.append(int(p))
            else:
                parts.append(0)
        return tuple(parts) if parts else (0,)

    @classmethod
    def _is_prefix_parent(cls, parent_number: str, child_number: str) -> bool:
        """True if ``parent_number`` is a strict prefix-parent of ``child_number``.

        ``3`` is a parent of ``3.2`` and ``3.2.1``; ``3.2`` is a parent of
        ``3.2.1``; ``3`` is NOT a parent of ``3`` (same) or of ``30``.
        """
        pp = cls._number_key(parent_number)
        cp = cls._number_key(child_number)
        return len(pp) < len(cp) and cp[: len(pp)] == pp

    def _build_required_tree(
        self, required_outline: List[Dict[str, Any]]
    ) -> List[Dict[str, Any]]:
        """Build a nested tree from the flat required-outline list.

        Returns a list of root nodes, each:
            {"number", "title", "level", "children": [ <node>, ... ]}
        Parent/child relationships are derived from the section *number* prefix
        (``3`` parents ``3.2``), so this is robust even if levels are absent or
        inconsistent. Nodes are sorted by number so parents precede children.
        """
        nodes = []
        for r in required_outline:
            nodes.append(
                {
                    "number": str(r.get("number", "") or ""),
                    "title": str(r.get("title", "") or ""),
                    "level": int(r.get("level", 1) or 1),
                    "children": [],
                }
            )
        nodes.sort(key=lambda n: self._number_key(n["number"]))

        roots: List[Dict[str, Any]] = []
        stack: List[Dict[str, Any]] = []
        for n in nodes:
            while stack and not self._is_prefix_parent(stack[-1]["number"], n["number"]):
                stack.pop()
            if stack:
                stack[-1]["children"].append(n)
            else:
                roots.append(n)
            stack.append(n)
        return roots

    def _titles_match(self, a: str, b: str) -> bool:
        """Compare two normalized titles for equivalence."""
        if not a:
            return True  # empty required title -> treat as present (no-op)
        if a == b:
            return True
        ratio = difflib.SequenceMatcher(None, a, b).ratio()
        return ratio >= 0.8 or a in b or b in a

    @staticmethod
    def _parse_leading_number(title: str) -> Optional[str]:
        """Extract a leading numeric label from a section *title*.

        ``"3.1 Makroumwelt"`` -> ``"3.1"``; ``"3. Darstellung"`` -> ``"3"``;
        ``"## 3.2 Foo"`` -> ``"3.2"``; ``"Makroumwelt"`` -> ``None``.
        Used to determine where a flat-emitted section belongs in the hierarchy
        (so a flat ``3.1`` can be recognised as a child of ``3``).
        """
        import re
        t = (title or "").lstrip("#").lstrip()
        m = re.match(r"(\d+(?:\.\d+)*)[.)]?\s*", t)
        return m.group(1) if m else None

    def _numbers_equal(self, a: Optional[str], b: Optional[str]) -> bool:
        """True if two section-number strings are equivalent (by tuple)."""
        if not a or not b:
            return False
        return self._number_key(a) == self._number_key(b)

    def _flatten_generated(
        self,
        sections: List[ReportSection],
        pool: List[Dict[str, Any]],
        original_parent: Optional[ReportSection],
    ) -> None:
        """Recursively flatten the generated outline into a flat pool.

        Each pool entry records the parsed leading number (from the title), the
        normalized title, the underlying ``ReportSection`` (identity preserved),
        a ``used`` flag, and the section it was originally nested under (so
        unclaimed extras can be re-attached in place).
        """
        for s in sections:
            pool.append(
                {
                    "number": self._parse_leading_number(s.title),
                    "norm_title": _normalize_title_for_match(s.title),
                    "section": s,
                    "used": False,
                    "original_parent": original_parent,
                }
            )
            if s.subsections:
                self._flatten_generated(s.subsections, pool, original_parent=s)

    def _claim_from_pool(
        self, rn: Dict[str, Any], pool: List[Dict[str, Any]]
    ) -> Optional[Dict[str, Any]]:
        """Claim the best-matching unused pool entry for required node ``rn``.

        Among unused entries whose title matches, an entry whose parsed number
        equals the required number is strongly preferred (disambiguates e.g.
        ``3 Darstellung`` from ``3.1 Darstellung``). Otherwise the first
        title-match is used — but ONLY if its number is hierarchy-compatible
        with the required number: a fuzzy/substring title match must NEVER claim
        a section at a different Gliederung level (review finding 1: required
        ``3 Darstellung`` wrongly claimed ``3.1 Darstellung der Methodik`` via
        substring, leaving the missing parent un-created and duplicating 3.1).
        """
        target = _normalize_title_for_match(rn["title"])
        req_number = rn.get("number") or ""
        first_title_match: Optional[Dict[str, Any]] = None
        for item in pool:
            if item["used"]:
                continue
            if not self._titles_match(target, item["norm_title"]):
                continue
            if self._numbers_equal(item.get("number"), req_number):
                return item  # exact number match wins
            # Fuzzy title fallback: reject if the candidate's number sits at a
            # different hierarchy level (one is a prefix-parent of the other).
            if self._numbers_compatible_for_fuzzy(req_number, item.get("number")):
                if first_title_match is None:
                    first_title_match = item
        return first_title_match

    def _numbers_compatible_for_fuzzy(
        self, req_number: Optional[str], cand_number: Optional[str]
    ) -> bool:
        """Whether a fuzzy title match may claim a candidate with this number.

        Returns False when both sides carry numbers and one is a strict
        prefix-parent of the other (i.e. they are at different Gliederung
        levels, like ``3`` vs ``3.1``), so a parent can never claim its child's
        slot (or vice versa) by title/substring similarity alone. When either
        number is absent we cannot judge the level, so the title match is
        allowed.
        """
        if not req_number or not cand_number:
            return True
        if self._numbers_equal(req_number, cand_number):
            return True
        if self._is_prefix_parent(req_number, cand_number) or self._is_prefix_parent(
            cand_number, req_number
        ):
            return False
        return True

    @staticmethod
    def _strategy_for_inserted(has_children: bool) -> str:
        """Research strategy for a freshly-inserted required section.

        Mirrors the validator's own rule (see ``_validate_and_correct_strategies``):
        a section WITH subsections synthesises from them; a leaf section does its
        own research. Because ``enforce_required_outline`` runs AFTER the final
        ``validate_and_correct``, we must set this correctly here rather than
        rely on the later pass (review finding 2).
        """
        return "synthesize_from_subsections" if has_children else "research_based"

    def _rebuild_from_required(
        self,
        required_nodes: List[Dict[str, Any]],
        pool: List[Dict[str, Any]],
        report: Dict[str, Any],
    ) -> List[ReportSection]:
        """Rebuild an outline section list from the required tree + global pool.

        For each required node (in Gliederung order):
          - claim its best match from the GLOBAL pool (any depth/position); if
            found, recurse to populate its subsections from the same pool and
            keep the claimed section;
          - else create a new ``ReportSection`` (strategy chosen by
            ``_strategy_for_inserted``) and populate its subsections from the
            pool (all new).

        Because the pool is global, this correctly handles required subsections
        that the planner emitted flat (as top-level siblings): they are claimed
        and moved into their matched parent instead of being duplicated.
        """
        if not required_nodes:
            return []

        result: List[ReportSection] = []
        for rn in required_nodes:
            claimed = self._claim_from_pool(rn, pool)
            if claimed is not None:
                claimed["used"] = True
                report["matched"].append(rn["title"])
                g = claimed["section"]
                # Reset subsections then rebuild from required children so the
                # hierarchy follows the required tree (flattens extras into the
                # pool for re-attachment).
                g.subsections = self._rebuild_from_required(
                    rn.get("children") or [], pool, report
                )
                result.append(g)
            else:
                report["missing_required"].append(rn["title"])
                report["inserted"].append(rn["title"])
                logger.warning(
                    "OutlineValidator: required section '%s' (%s, level %d) was "
                    "MISSING from the generated outline; inserting it "
                    "deterministically.",
                    rn["title"],
                    rn["number"],
                    rn["level"],
                )
                new_sec = ReportSection(
                    section_id=(
                        rn["title"].lower().replace(" ", "_")[:40]
                        or f"section_{rn['number']}"
                    ),
                    title=rn["title"],
                    description=(
                        f"Required section from the user's Gliederung "
                        f"({rn['number']}). Cover the topics specified in the "
                        f"briefing for this section."
                    ),
                    research_strategy=self._strategy_for_inserted(
                        bool(rn.get("children"))
                    ),
                    subsections=[],
                )
                if rn.get("children"):
                    new_sec.subsections = self._rebuild_from_required(
                        rn["children"], pool, report
                    )
                result.append(new_sec)
        return result

    def _reattach_extras(
        self, outline: List[ReportSection], pool: List[Dict[str, Any]]
    ) -> None:
        """Re-attach unclaimed generated sections (LLM extras) in place.

        An extra that was originally nested under a parent that is still present
        in the rebuilt outline is appended to that parent's subsections; a
        top-level extra is appended to the top-level list.

        Crucially (review finding 2), when a section is claimed and moved into
        the required tree, it must NOT also remain inside its original extra
        container. We therefore PRUNE claimed (used) descendants from every
        extra subtree before re-attaching it, and an unused section whose parent
        is itself an unused extra is reached only via that parent's pruned
        subtree (it is not independently hoisted to the top level — the previous
        implementation did exactly that, duplicating 3.4.1 Detail).

        Mutates ``outline`` (top-level list) and matched sections'
        ``subsections`` in place.
        """
        if not pool:
            return

        used_ids: Set[int] = {id(it["section"]) for it in pool if it["used"]}

        # Map id -> section for every section currently in the rebuilt tree, so
        # nested extras can be appended to their matched parent.
        present_by_id: Dict[int, ReportSection] = {}

        def _collect(sections: List[ReportSection]) -> None:
            for s in sections:
                present_by_id[id(s)] = s
                if s.subsections:
                    _collect(s.subsections)

        _collect(outline)

        # Identify which extras are FOREST ROOTS: an unused section whose
        # original parent is None (top-level) or USED (a matched parent that is
        # still present -> nested extra). Unused sections whose parent is ALSO
        # unused are NOT roots: they are reached through their parent's pruned
        # subtree, which avoids duplicating them at the top level.
        top_level_extras: List[ReportSection] = []
        nested_extras: Dict[int, List[ReportSection]] = defaultdict(list)
        for item in pool:
            if item["used"]:
                continue
            parent = item.get("original_parent")
            if parent is None:
                top_level_extras.append(item["section"])
            elif id(parent) in used_ids:
                # Parent was claimed -> it is present in the rebuilt tree.
                nested_extras[id(parent)].append(item["section"])
            # else: parent is unused -> reached via parent's pruned subtree.

        # Prune claimed descendants from a section's subtree, in place.
        def _prune_claimed(section: ReportSection) -> bool:
            """Remove used descendants; return True if the section should be
            KEPT, False if it became an empty container (originally had
            children, all of which were claimed) and should be dropped."""
            had_children = bool(section.subsections)
            kept: List[ReportSection] = []
            for child in section.subsections:
                if id(child) in used_ids:
                    continue  # claimed elsewhere -> drop
                if _prune_claimed(child):
                    kept.append(child)
                # else: child became an empty container -> drop
            section.subsections = kept
            # Drop a section that was a pure container and is now empty.
            return bool(section.subsections) or not had_children

        # Prune + filter top-level extras.
        pruned_top: List[ReportSection] = []
        for sec in top_level_extras:
            if _prune_claimed(sec):
                pruned_top.append(sec)

        # Prune + filter nested extras.
        pruned_nested: Dict[int, List[ReportSection]] = defaultdict(list)
        for parent_id, secs in nested_extras.items():
            for sec in secs:
                if _prune_claimed(sec):
                    pruned_nested[parent_id].append(sec)

        # Append nested extras back under their (still-present) matched parent.
        for parent_id, secs in pruned_nested.items():
            parent = present_by_id.get(parent_id)
            if parent is not None:
                parent.subsections.extend(secs)

        # Append top-level extras at the end.
        if pruned_top:
            outline.extend(pruned_top)

    def _calculate_max_depth(self, outline: List[ReportSection], current_depth: int = 0) -> int:
        """Calculate the maximum depth of the outline."""
        if not outline:
            return current_depth
        
        max_depth = current_depth
        for section in outline:
            if section.subsections:
                subsection_depth = self._calculate_max_depth(section.subsections, current_depth + 1)
                max_depth = max(max_depth, subsection_depth)
        
        return max_depth
    
    def _count_sections(self, outline: List[ReportSection]) -> int:
        """Count total number of sections in the outline."""
        count = len(outline)
        for section in outline:
            if section.subsections:
                count += self._count_sections(section.subsections)
        return count
    
    def _check_depth(self, outline: List[ReportSection], auto_correct: bool, current_depth: int = 0) -> List[ReportSection]:
        """
        Check and fix depth violations.
        
        Args:
            outline: The outline to check
            auto_correct: Whether to apply corrections
            current_depth: Current depth level (0 = top-level)
        """
        corrected_outline = []
        
        for section in outline:
            if current_depth >= self.max_depth:
                # This section exceeds max depth
                issue = {
                    "type": "depth_exceeded",
                    "section_id": section.section_id,
                    "section_title": section.title,
                    "depth": current_depth,
                    "max_allowed": self.max_depth
                }
                self.validation_issues.append(issue)
                
                if auto_correct:
                    # Don't include sections that exceed max depth
                    correction = {
                        "type": "removed_deep_section",
                        "section_id": section.section_id,
                        "section_title": section.title,
                        "depth": current_depth
                    }
                    self.corrections_made.append(correction)
                    logger.warning(f"Removed section '{section.title}' (ID: {section.section_id}) - exceeded max depth {self.max_depth}")
                    continue  # Skip this section
            else:
                # Process subsections recursively
                if section.subsections and current_depth + 1 < self.max_depth:
                    section.subsections = self._check_depth(section.subsections, auto_correct, current_depth + 1)
                elif section.subsections and current_depth + 1 >= self.max_depth:
                    # Subsections would exceed depth, flatten or remove them
                    issue = {
                        "type": "subsections_exceed_depth",
                        "section_id": section.section_id,
                        "section_title": section.title,
                        "subsection_count": len(section.subsections)
                    }
                    self.validation_issues.append(issue)
                    
                    if auto_correct:
                        # Merge subsection descriptions into parent
                        subsection_info = []
                        for subsec in section.subsections:
                            subsection_info.append(f"- {subsec.title}: {subsec.description}")
                        
                        if subsection_info:
                            section.description += "\n\nKey subtopics to cover:\n" + "\n".join(subsection_info)
                        
                        section.subsections = []  # Remove subsections
                        
                        correction = {
                            "type": "flattened_subsections",
                            "section_id": section.section_id,
                            "section_title": section.title,
                            "merged_count": len(subsection_info)
                        }
                        self.corrections_made.append(correction)
                        logger.info(f"Flattened {len(subsection_info)} subsections of '{section.title}' into description")
                
                corrected_outline.append(section)
        
        return corrected_outline
    
    def _check_duplicates(self, outline: List[ReportSection], auto_correct: bool) -> List[ReportSection]:
        """
        Check and fix duplicate sections based on title similarity.
        """
        def normalize_title(title: str) -> str:
            """Normalize title for comparison."""
            return title.lower().strip().replace("-", " ").replace("_", " ")
        
        def calculate_similarity(title1: str, title2: str) -> float:
            """Calculate similarity between two titles."""
            return difflib.SequenceMatcher(None, normalize_title(title1), normalize_title(title2)).ratio()
        
        # First pass: collect all sections with their paths
        all_sections = []
        
        def collect_sections(sections: List[ReportSection], path: str = ""):
            for i, section in enumerate(sections):
                section_path = f"{path}/{i}" if path else str(i)
                all_sections.append({
                    "section": section,
                    "path": section_path,
                    "normalized_title": normalize_title(section.title)
                })
                if section.subsections:
                    collect_sections(section.subsections, section_path)
        
        collect_sections(outline)
        
        # Find duplicates (similarity > 0.85)
        duplicates = defaultdict(list)
        processed = set()
        
        for i, item1 in enumerate(all_sections):
            if i in processed:
                continue
                
            similar_group = [item1]
            for j, item2 in enumerate(all_sections[i+1:], start=i+1):
                if j in processed:
                    continue
                    
                similarity = calculate_similarity(item1["section"].title, item2["section"].title)
                if similarity > 0.85:  # High similarity threshold
                    similar_group.append(item2)
                    processed.add(j)
            
            if len(similar_group) > 1:
                processed.add(i)
                # Group by the first item's title as key
                duplicates[item1["section"].title] = similar_group
        
        # Report and fix duplicates
        for title, duplicate_group in duplicates.items():
            issue = {
                "type": "duplicate",
                "title": title,
                "count": len(duplicate_group),
                "sections": [{"id": item["section"].section_id, "title": item["section"].title} 
                           for item in duplicate_group]
            }
            self.validation_issues.append(issue)
            
            if auto_correct and len(duplicate_group) > 1:
                # Keep the first occurrence, merge content from others
                primary = duplicate_group[0]["section"]
                
                # Merge descriptions and notes from duplicates
                merged_descriptions = [primary.description]
                merged_note_ids = set(primary.associated_note_ids or [])
                
                for item in duplicate_group[1:]:
                    dup_section = item["section"]
                    if dup_section.description and dup_section.description not in merged_descriptions:
                        merged_descriptions.append(dup_section.description)
                    if dup_section.associated_note_ids:
                        merged_note_ids.update(dup_section.associated_note_ids)
                
                # Update primary section with merged content
                if len(merged_descriptions) > 1:
                    primary.description = "\n\n".join(merged_descriptions)
                if merged_note_ids:
                    primary.associated_note_ids = list(merged_note_ids)
                
                correction = {
                    "type": "merged_duplicates",
                    "primary_section": primary.section_id,
                    "merged_count": len(duplicate_group) - 1,
                    "merged_sections": [item["section"].section_id for item in duplicate_group[1:]]
                }
                self.corrections_made.append(correction)
                logger.info(f"Merged {len(duplicate_group)-1} duplicate sections into '{primary.title}'")
        
        # Now remove the duplicate sections from the outline
        if auto_correct and duplicates:
            sections_to_remove = set()
            for duplicate_group in duplicates.values():
                # Mark all but the first for removal
                for item in duplicate_group[1:]:
                    sections_to_remove.add(item["section"].section_id)
            
            outline = self._remove_sections_by_id(outline, sections_to_remove)
        
        return outline
    
    def _remove_sections_by_id(self, outline: List[ReportSection], ids_to_remove: Set[str]) -> List[ReportSection]:
        """Remove sections with specified IDs from the outline."""
        filtered_outline = []
        
        for section in outline:
            if section.section_id not in ids_to_remove:
                # Keep this section, but filter its subsections
                if section.subsections:
                    section.subsections = self._remove_sections_by_id(section.subsections, ids_to_remove)
                filtered_outline.append(section)
        
        return filtered_outline
    
    def _check_empty_sections(self, outline: List[ReportSection], auto_correct: bool) -> List[ReportSection]:
        """Check for and optionally remove empty sections."""
        filtered_outline = []
        
        for section in outline:
            is_empty = (
                not section.title or 
                section.title.strip() == "" or
                (not section.description or section.description.strip() == "") and 
                not section.subsections
            )
            
            if is_empty:
                issue = {
                    "type": "empty_section",
                    "section_id": section.section_id,
                    "title": section.title or "(no title)"
                }
                self.validation_issues.append(issue)
                
                if auto_correct:
                    correction = {
                        "type": "removed_empty_section",
                        "section_id": section.section_id
                    }
                    self.corrections_made.append(correction)
                    logger.info(f"Removed empty section with ID: {section.section_id}")
                    continue  # Skip this section
            
            # Recursively check subsections
            if section.subsections:
                section.subsections = self._check_empty_sections(section.subsections, auto_correct)
            
            filtered_outline.append(section)
        
        return filtered_outline
    
    def _ensure_unique_ids(self, outline: List[ReportSection], auto_correct: bool) -> List[ReportSection]:
        """Ensure all section IDs are unique."""
        seen_ids = set()
        id_counter = defaultdict(int)
        
        def process_section(section: ReportSection):
            if section.section_id in seen_ids:
                issue = {
                    "type": "duplicate_id",
                    "section_id": section.section_id,
                    "title": section.title
                }
                self.validation_issues.append(issue)
                
                if auto_correct:
                    # Generate a new unique ID
                    base_id = section.section_id
                    id_counter[base_id] += 1
                    new_id = f"{base_id}_v{id_counter[base_id]}"
                    
                    correction = {
                        "type": "renamed_duplicate_id",
                        "old_id": section.section_id,
                        "new_id": new_id,
                        "title": section.title
                    }
                    self.corrections_made.append(correction)
                    
                    section.section_id = new_id
                    logger.info(f"Renamed duplicate section ID from '{base_id}' to '{new_id}'")
            
            seen_ids.add(section.section_id)
            
            # Process subsections
            if section.subsections:
                for subsection in section.subsections:
                    process_section(subsection)
        
        for section in outline:
            process_section(section)
        
        return outline
    
    def _has_research_based_section(self, outline: List[ReportSection]) -> bool:
        """Check if outline has at least one research-based section."""
        for section in outline:
            if section.research_strategy == "research_based":
                return True
            if section.subsections and self._has_research_based_section(section.subsections):
                return True
        return False
    
    def _remove_references_sections(self, outline: List[ReportSection], auto_correct: bool) -> List[ReportSection]:
        """
        Remove References/Bibliography/Citations sections as they're auto-generated.
        
        Args:
            outline: The outline to check
            auto_correct: Whether to apply corrections
            
        Returns:
            Outline with references sections removed
        """
        if not auto_correct:
            # Just report issues without removing
            for section in outline:
                title_lower = section.title.lower() if section.title else ""
                if any(term in title_lower for term in ['references', 'bibliography', 'citations', 'works cited']):
                    self.validation_issues.append({
                        "type": "references_section",
                        "title": section.title,
                        "message": "References sections should not be included (they're auto-generated)"
                    })
            return outline
        
        # Remove references sections
        filtered_outline = []
        for section in outline:
            title_lower = section.title.lower() if section.title else ""
            if any(term in title_lower for term in ['references', 'bibliography', 'citations', 'works cited']):
                self.corrections_made.append({
                    "type": "removed_references",
                    "section_title": section.title,
                    "reason": "References sections are auto-generated"
                })
                logger.info(f"Removed references section: '{section.title}'")
            else:
                # Process subsections recursively
                if section.subsections:
                    section.subsections = self._remove_references_sections(section.subsections, auto_correct)
                filtered_outline.append(section)
        
        return filtered_outline
    
    def _validate_and_correct_strategies(self, outline: List[ReportSection], auto_correct: bool, is_top_level: bool = True) -> List[ReportSection]:
        """
        Validate and correct research strategies based on section characteristics.
        
        Rules:
        1. Respect content_based assignments for first/last TOP-LEVEL sections only
        2. Subsections (non-top-level) should NEVER be content_based - always research_based or synthesize_from_subsections
        3. Sections with subsections should use 'synthesize_from_subsections'
        4. Check first/last sections with research_based against intro/conclusion keywords
        5. Leaf sections (no subsections) default to 'research_based' unless they're intro/conclusion
        6. At least one section must be 'research_based'
        """
        if not outline:
            return outline
        
        # Extended keywords for better detection
        intro_keywords = ["introduction", "intro", "overview", "background", "preface", 
                          "prologue", "proclamation", "announcement", "declaration", "opening", 
                          "beginning", "commencement", "foreword", "preamble", "kickoff"]
        
        conclusion_keywords = ["conclusion", "summary", "discussion", "future", "implications", 
                               "final", "closing", "epilogue", "farewell", "reflection", "wrap-up", 
                               "ending", "afterword", "retrospective", "outlook"]
        
        # Track if we have at least one research_based section
        has_research_based = False
        
        for i, section in enumerate(outline):
            title_lower = section.title.lower() if section.title else ""
            is_first_section = (i == 0)
            is_last_section = (i == len(outline) - 1)
            current_strategy = section.research_strategy
            
            # Determine expected strategy based on section characteristics
            expected_strategy = None
            
            # NEW Rule: Subsections should NEVER be content_based
            if not is_top_level and current_strategy == "content_based":
                # Force subsections to be research_based (or synthesize_from_subsections if they have children)
                if section.subsections and len(section.subsections) > 0:
                    expected_strategy = "synthesize_from_subsections"
                else:
                    expected_strategy = "research_based"
                    has_research_based = True
                logger.info(f"Subsection '{section.title}' was content_based, changing to {expected_strategy}")
            
            # Rule 1: Respect content_based for first/last TOP-LEVEL sections only
            elif is_top_level and current_strategy == "content_based" and (is_first_section or is_last_section):
                # Trust the planning agent's judgment for first/last TOP-LEVEL sections with content_based
                expected_strategy = "content_based"
            
            # Rule 2: Sections with subsections
            elif section.subsections and len(section.subsections) > 0:
                expected_strategy = "synthesize_from_subsections"
            
            # Rule 3: Check first section with research_based against intro keywords
            elif is_first_section and current_strategy == "research_based":
                # Check if it looks like an intro (keywords or section_id)
                if any(keyword in title_lower for keyword in intro_keywords):
                    expected_strategy = "content_based"
                elif section.section_id and "intro" in section.section_id.lower():
                    expected_strategy = "content_based"
                else:
                    # Keep research_based if it doesn't look like an intro
                    expected_strategy = "research_based"
                    has_research_based = True
            
            # Rule 4: Check last section with research_based against conclusion keywords
            elif is_last_section and current_strategy == "research_based":
                # Check if it looks like a conclusion
                if any(keyword in title_lower for keyword in conclusion_keywords):
                    expected_strategy = "content_based"
                elif section.section_id and any(word in section.section_id.lower() for word in ["conclusion", "summary", "final"]):
                    expected_strategy = "content_based"
                else:
                    # Keep research_based if it doesn't look like a conclusion
                    expected_strategy = "research_based"
                    has_research_based = True
            
            # Rule 5: For middle sections - apply existing logic
            # Check if it's an intro/conclusion by keywords (regardless of position)
            elif any(keyword in title_lower for keyword in intro_keywords):
                expected_strategy = "content_based"
            
            elif any(keyword in title_lower for keyword in conclusion_keywords):
                expected_strategy = "content_based"
            
            # Rule 6: Leaf sections (default to research_based)
            else:
                expected_strategy = "research_based"
                has_research_based = True
            
            # Check if current strategy matches expected
            if section.research_strategy != expected_strategy:
                issue = {
                    "type": "incorrect_strategy",
                    "section_id": section.section_id,
                    "section_title": section.title,
                    "current_strategy": section.research_strategy,
                    "expected_strategy": expected_strategy,
                    "reason": self._get_strategy_reason(section, i, len(outline))
                }
                self.validation_issues.append(issue)
                
                if auto_correct:
                    old_strategy = section.research_strategy
                    section.research_strategy = expected_strategy
                    
                    correction = {
                        "type": "strategy_corrected",
                        "section_id": section.section_id,
                        "section_title": section.title,
                        "old_strategy": old_strategy,
                        "new_strategy": expected_strategy
                    }
                    self.corrections_made.append(correction)
                    logger.info(f"Corrected strategy for '{section.title}': {old_strategy} -> {expected_strategy}")
            
            # Track if this section is research_based
            if section.research_strategy == "research_based":
                has_research_based = True
            
            # Recursively check subsections (passing is_top_level=False)
            if section.subsections:
                section.subsections = self._validate_and_correct_strategies(section.subsections, auto_correct, is_top_level=False)
                # Check if any subsection is research_based
                if self._has_research_based_section(section.subsections):
                    has_research_based = True
        
        # Rule 5: Ensure at least one research_based section exists
        if not has_research_based and auto_correct and outline:
            # Find a suitable section to make research_based
            # Prefer middle sections that are currently leaf nodes
            for section in outline:
                if not section.subsections and section.research_strategy != "content_based":
                    title_lower = section.title.lower() if section.title else ""
                    # Avoid intro/conclusion sections
                    if not any(keyword in title_lower for keyword in ["introduction", "conclusion", "summary", "discussion"]):
                        old_strategy = section.research_strategy
                        section.research_strategy = "research_based"
                        
                        correction = {
                            "type": "forced_research_based",
                            "section_id": section.section_id,
                            "section_title": section.title,
                            "old_strategy": old_strategy,
                            "reason": "No research_based sections found, converting one section"
                        }
                        self.corrections_made.append(correction)
                        logger.warning(f"No research_based sections found. Converted '{section.title}' to research_based")
                        break
        
        return outline
    
    def _get_strategy_reason(self, section: ReportSection, index: int, total: int) -> str:
        """Get a human-readable reason for the expected strategy."""
        title_lower = section.title.lower() if section.title else ""
        is_first_section = (index == 0)
        is_last_section = (index == total - 1)
        current_strategy = section.research_strategy
        
        # Extended keywords (matching the ones in _validate_and_correct_strategies)
        intro_keywords = ["introduction", "intro", "overview", "background", "preface", 
                          "prologue", "proclamation", "announcement", "declaration", "opening", 
                          "beginning", "commencement", "foreword", "preamble", "kickoff"]
        
        conclusion_keywords = ["conclusion", "summary", "discussion", "future", "implications", 
                               "final", "closing", "epilogue", "farewell", "reflection", "wrap-up", 
                               "ending", "afterword", "retrospective", "outlook"]
        
        if current_strategy == "content_based" and (is_first_section or is_last_section):
            return "Planning agent's content_based assignment respected for first/last sections"
        elif is_first_section and any(keyword in title_lower for keyword in intro_keywords):
            return "First section appears to be an introduction"
        elif is_last_section and any(keyword in title_lower for keyword in conclusion_keywords):
            return "Last section appears to be a conclusion"
        elif any(keyword in title_lower for keyword in intro_keywords):
            return "Section title suggests introduction content"
        elif any(keyword in title_lower for keyword in conclusion_keywords):
            return "Section title suggests conclusion/summary content"
        elif section.subsections:
            return "Sections with subsections should synthesize from them"
        else:
            return "Leaf sections should conduct research"


def create_reflection_prompt(
    outline: List[ReportSection], 
    validation_report: Dict[str, Any],
    mission_goal: str
) -> str:
    """
    Create a prompt for the reflection agent to review and improve the outline.
    
    Args:
        outline: The current outline
        validation_report: Validation report with issues and corrections
        mission_goal: The overall mission goal
        
    Returns:
        A formatted prompt for reflection
    """
    prompt = f"""You are reviewing a research outline for quality and structure.

**Mission Goal:** {mission_goal}

**Current Outline Issues Found:**
"""
    
    if validation_report["issues"]:
        for issue in validation_report["issues"]:
            if issue["type"] == "duplicate":
                prompt += f"- Duplicate sections found: '{issue['title']}' appears {issue['count']} times\n"
            elif issue["type"] == "depth_exceeded":
                prompt += f"- Section '{issue['section_title']}' exceeds maximum depth of {issue['max_allowed']}\n"
            elif issue["type"] == "empty_section":
                prompt += f"- Empty section found: {issue['title']}\n"
            elif issue["type"] == "duplicate_id":
                prompt += f"- Duplicate section ID: {issue['section_id']}\n"
    else:
        prompt += "- No structural issues found\n"
    
    prompt += f"""
**Outline Statistics:**
- Maximum depth setting: {validation_report['max_depth_setting']} levels
- Actual maximum depth: {validation_report['actual_max_depth']} levels
- Total sections: {validation_report['total_sections']}
- Duplicate sections found: {validation_report['duplicate_sections_found']}

**Corrections Applied:**
"""
    
    if validation_report["corrections"]:
        for correction in validation_report["corrections"]:
            if correction["type"] == "merged_duplicates":
                prompt += f"- Merged {correction['merged_count']} duplicate sections into section {correction['primary_section']}\n"
            elif correction["type"] == "removed_deep_section":
                prompt += f"- Removed section '{correction['section_title']}' that exceeded depth limit\n"
            elif correction["type"] == "flattened_subsections":
                prompt += f"- Flattened {correction['merged_count']} subsections of '{correction['section_title']}'\n"
    else:
        prompt += "- No automatic corrections were needed\n"
    
    prompt += """

**Your Task:**
Review the corrected outline and provide suggestions for improvement:

1. **Content Coverage**: Are all aspects of the mission goal adequately covered?
2. **Logical Flow**: Does the sequence of sections make logical sense?
3. **Balance**: Are sections appropriately balanced in scope and detail?
4. **Redundancy**: Are there any remaining redundancies or overlaps?
5. **Gaps**: Are there any missing topics that should be included?

Provide specific, actionable suggestions for improving the outline structure and content coverage.
"""
    
    return prompt