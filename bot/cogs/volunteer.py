import os
import re
import json
import uuid

import discord
from discord.ext import commands

from bot.utils.formatting import create_base_embed, IEEE_BLUE

# Volunteer board: tasks posted with Claim/Unclaim buttons that any member can
# press. Officers add and close tasks from /officer chapter.
# The buttons' custom IDs carry the task ID, so they keep working after restarts.
TASKS_FILE = "data/tasks.json"


def load_tasks():
    if not os.path.exists(TASKS_FILE):
        return {}
    with open(TASKS_FILE) as f:
        return json.load(f)

def save_tasks(tasks):
    os.makedirs(os.path.dirname(TASKS_FILE), exist_ok=True)
    tmp = TASKS_FILE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(tasks, f, indent=2)
    os.replace(tmp, TASKS_FILE)

def new_task(title, when, details, slots, created_by, key=None):
    return {"id": uuid.uuid4().hex[:10], "title": title[:200], "when": when or "", "details": details or "",
            "slots": max(1, slots), "claimed": [], "created_by": created_by, "key": key,
            "open": True, "channel_id": None, "message_id": None}

def task_embed(task):
    filled = len(task["claimed"])
    status = ("✅ Closed" if not task["open"] else
              "🟢 Fully staffed" if filled >= task["slots"] else f"🟡 {task['slots'] - filled} spot(s) open")
    lines = []
    if task["when"]:
        lines.append(f"**When:** {task['when']}")
    if task["details"]:
        lines.append(task["details"])
    lines.append(f"**Volunteers ({filled}/{task['slots']}):** " + (", ".join(f"<@{u}>" for u in task["claimed"]) or "nobody yet"))
    lines.append(status)
    embed = create_base_embed(title=f"🙋 {task['title']}", description="\n".join(lines), color=IEEE_BLUE)
    embed.set_footer(text="Press Claim to sign up · IEEE CS at UCF")
    return embed

def task_view(task):
    view = discord.ui.View(timeout=None)
    if task["open"]:
        full = len(task["claimed"]) >= task["slots"]
        view.add_item(TaskButton("claim", task["id"], disabled=full))
        view.add_item(TaskButton("unclaim", task["id"]))
    return view


class TaskButton(discord.ui.DynamicItem[discord.ui.Button], template=r'task:(?P<action>claim|unclaim):(?P<id>[0-9a-f]+)'):
    def __init__(self, action, task_id, disabled=False):
        label, style = ("Claim", discord.ButtonStyle.success) if action == "claim" else ("Unclaim", discord.ButtonStyle.secondary)
        super().__init__(discord.ui.Button(label=label, style=style, custom_id=f"task:{action}:{task_id}", disabled=disabled))
        self.action, self.task_id = action, task_id

    @classmethod
    async def from_custom_id(cls, interaction, item, match: re.Match):
        return cls(match["action"], match["id"])

    async def callback(self, interaction: discord.Interaction):
        tasks = load_tasks()
        task = tasks.get(self.task_id)
        if task is None or not task["open"]:
            await interaction.response.send_message("This task is closed.", ephemeral=True)
            return
        user = interaction.user.id
        if self.action == "claim":
            if user in task["claimed"]:
                note = "You're already signed up."
            elif len(task["claimed"]) >= task["slots"]:
                note = "It's full, sorry!"
            else:
                task["claimed"].append(user)
                note = f"You're signed up for **{task['title']}**. Thank you! 💙"
        else:
            if user in task["claimed"]:
                task["claimed"].remove(user)
                note = "You've been taken off this task."
            else:
                note = "You weren't signed up."
        save_tasks(tasks)
        await interaction.response.edit_message(embed=task_embed(task), view=task_view(task))
        await interaction.followup.send(note, ephemeral=True)
        print(f"Task {task['title']}: {self.action} by {interaction.user} ({user})")


class Volunteer(commands.Cog):
    """Registers the task buttons so they respond after restarts."""

    def __init__(self, bot):
        self.bot = bot
        bot.add_dynamic_items(TaskButton)

    def cog_unload(self):
        self.bot.remove_dynamic_items(TaskButton)


async def setup(bot):
    await bot.add_cog(Volunteer(bot))
