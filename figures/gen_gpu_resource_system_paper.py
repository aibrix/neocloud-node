#!/usr/bin/env python3
"""Generate a publication-style system diagram for the GPU resource manager."""

from __future__ import annotations

import matplotlib.pyplot as plt
from matplotlib.patches import FancyArrowPatch, Rectangle


FIG = "figures/gpu-resource-system-paper"


def box(ax, xy, wh, text, *, fc="#ffffff", ec="#222222", lw=1.1, fontsize=9):
    x, y = xy
    w, h = wh
    patch = Rectangle((x, y), w, h, linewidth=lw, edgecolor=ec, facecolor=fc)
    ax.add_patch(patch)
    ax.text(
        x + w / 2,
        y + h / 2,
        text,
        ha="center",
        va="center",
        fontsize=fontsize,
        linespacing=1.12,
    )
    return patch


def arrow(ax, start, end, *, color="#222222", lw=1.0, style="-", rad=0.0):
    patch = FancyArrowPatch(
        start,
        end,
        arrowstyle="-|>",
        mutation_scale=8,
        linewidth=lw,
        linestyle=style,
        color=color,
        connectionstyle=f"arc3,rad={rad}",
        shrinkA=2,
        shrinkB=2,
    )
    ax.add_patch(patch)
    return patch


def label(ax, x, y, text, *, fontsize=9, weight="bold"):
    ax.text(x, y, text, ha="center", va="center", fontsize=fontsize, fontweight=weight)


def main():
    plt.rcParams.update(
        {
            "font.family": "serif",
            "font.serif": ["Times New Roman", "Times", "DejaVu Serif"],
            "font.size": 9,
            "pdf.fonttype": 42,
            "ps.fonttype": 42,
        }
    )

    fig, ax = plt.subplots(figsize=(12.0, 4.8))
    ax.set_xlim(0, 1)
    ax.set_ylim(0, 1)
    ax.axis("off")

    blue = "#e8f1fb"
    blue_edge = "#1f4e79"
    tan = "#f8efe4"
    gray = "#f5f5f5"

    # Top market/policy signals.
    label(ax, 0.50, 0.955, "Market and Policy Signals")
    catalog = box(ax, (0.305, 0.865), (0.13, 0.06), "Offer Catalog\nnative APIs + observations", fc=gray, fontsize=7.5)
    price = box(ax, (0.450, 0.865), (0.13, 0.06), "Price / Risk\nestimate + billing", fc=gray, fontsize=7.5)
    policy = box(ax, (0.595, 0.865), (0.13, 0.06), "Policy\nbudget, spot, deadline", fc=gray, fontsize=7.5)
    failures = box(ax, (0.740, 0.865), (0.13, 0.06), "Failure Signals\nattempts + scorecard", fc=gray, fontsize=7.5)

    # Workload APIs.
    label(ax, 0.08, 0.78, "Workload APIs")
    pod = box(ax, (0.03, 0.665), (0.10, 0.055), "Kubernetes\nPod", fontsize=8)
    batch = box(ax, (0.03, 0.590), (0.10, 0.055), "AIBrix\nBatchJob", fontsize=8)
    serving = box(ax, (0.03, 0.515), (0.10, 0.055), "AIBrix\nServing", fontsize=8)

    intent = box(ax, (0.18, 0.59), (0.12, 0.095), "Resource Intent\nGPU, budget,\nSLO, recovery", fc=blue, ec=blue_edge, fontsize=8)

    # Shared manager boundary.
    ax.add_patch(Rectangle((0.34, 0.36), 0.33, 0.34, linewidth=1.1, edgecolor="#666666", facecolor="#fbfbfb"))
    label(ax, 0.505, 0.68, "Shared GPU Resource Manager", fontsize=8)
    filter_box = box(ax, (0.365, 0.535), (0.075, 0.07), "Capability\nFilter", fc=blue, ec=blue_edge, fontsize=8)
    planner = box(ax, (0.465, 0.535), (0.085, 0.07), "Placement\nPlanner", fc=blue, ec=blue_edge, fontsize=8)
    plan = box(ax, (0.575, 0.535), (0.075, 0.07), "Acquire\nPlan", fc=blue, ec=blue_edge, fontsize=8)
    orchestrator = box(ax, (0.465, 0.395), (0.12, 0.075), "Acquire Orchestrator\nidempotency, retry,\nfallback", fc=tan, ec="#7a4a12", fontsize=8)

    adapters = box(ax, (0.72, 0.535), (0.10, 0.07), "Provider\nAdapters", fc=tan, ec="#7a4a12", fontsize=8)

    # Providers.
    label(ax, 0.91, 0.78, "Cloud GPU Providers")
    runpod = box(ax, (0.87, 0.665), (0.10, 0.055), "RunPod\nPods", fontsize=8)
    lamb = box(ax, (0.87, 0.590), (0.10, 0.055), "Lambda\nVMs", fontsize=8)
    other = box(ax, (0.87, 0.515), (0.10, 0.055), "Vast /\nCloud VMs", fontsize=8)

    observe = box(ax, (0.735, 0.245), (0.14, 0.075), "Observation Loop\nupdates catalog,\ncost, failures", fc=gray, fontsize=8)

    # Durable surfaces.
    label(ax, 0.50, 0.215, "Durable State and User Surfaces")
    store = box(ax, (0.26, 0.105), (0.13, 0.065), "Binding /\nJobStore", fc=gray, fontsize=8)
    attempts = box(ax, (0.435, 0.105), (0.13, 0.065), "Attempts +\nConditions", fc=gray, fontsize=8)
    metrics = box(ax, (0.61, 0.105), (0.13, 0.065), "Cost + Events\n+ Metrics", fc=gray, fontsize=8)

    # Main data flow.
    for source in (pod, batch, serving):
        sx = source.get_x() + source.get_width()
        sy = source.get_y() + source.get_height() / 2
        arrow(ax, (sx, sy), (intent.get_x(), intent.get_y() + intent.get_height() / 2), rad=0.0)

    arrow(ax, (0.30, 0.637), (0.365, 0.570))
    arrow(ax, (0.440, 0.570), (0.465, 0.570))
    arrow(ax, (0.550, 0.570), (0.575, 0.570))
    arrow(ax, (0.612, 0.535), (0.525, 0.470), rad=0.12)
    arrow(ax, (0.585, 0.432), (0.720, 0.570))

    for target in (runpod, lamb, other):
        arrow(ax, (0.820, 0.570), (target.get_x(), target.get_y() + target.get_height() / 2), rad=0.0)

    # Signals into planner.
    arrow(ax, (0.370, 0.865), (0.400, 0.605), rad=0.05)
    arrow(ax, (0.515, 0.865), (0.505, 0.605), rad=0.00)
    arrow(ax, (0.660, 0.865), (0.505, 0.605), rad=-0.06)
    arrow(ax, (0.805, 0.865), (0.505, 0.605), rad=-0.12)

    # State writes.
    arrow(ax, (0.490, 0.395), (0.325, 0.170), rad=0.12)
    arrow(ax, (0.525, 0.395), (0.500, 0.170), rad=0.00)
    arrow(ax, (0.560, 0.395), (0.675, 0.170), rad=-0.12)

    # Observation loop and feedback.
    for source in (runpod, lamb, other):
        arrow(
            ax,
            (source.get_x() + source.get_width() / 2, source.get_y()),
            (observe.get_x() + observe.get_width() / 2, observe.get_y() + observe.get_height()),
            rad=-0.04,
        )
    arrow(ax, (0.805, 0.320), (0.805, 0.865), color="#555555", style="--", rad=0.05)
    arrow(ax, (0.735, 0.260), (0.565, 0.140), color="#555555", style="--", rad=0.05)

    fig.savefig(f"{FIG}.pdf", bbox_inches="tight", pad_inches=0.04)
    fig.savefig(f"{FIG}.svg", bbox_inches="tight", pad_inches=0.04)
    fig.savefig(f"{FIG}.png", dpi=300, bbox_inches="tight", pad_inches=0.04)


if __name__ == "__main__":
    main()
