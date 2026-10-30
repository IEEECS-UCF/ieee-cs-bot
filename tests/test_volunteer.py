import types

from bot.cogs import volunteer


class Response:
    def __init__(self):
        self.edited = None
    async def edit_message(self, **kwargs):
        self.edited = kwargs

class Followup:
    def __init__(self):
        self.notes = []
    async def send(self, text, **kwargs):
        self.notes.append(text)

def click(task_id, action, user):
    interaction = types.SimpleNamespace(user=types.SimpleNamespace(id=user, __str__=lambda s: "u"),
                                        response=Response(), followup=Followup())
    return volunteer.TaskButton(action, task_id), interaction

async def test_claim_fills_spots_and_disables():
    task = volunteer.new_task("Table at the fair", "Fri 3-5", "", 2, created_by=0)
    volunteer.save_tasks({task["id"]: task})
    for user in (10, 11, 12):
        button, interaction = click(task["id"], "claim", user)
        await button.callback(interaction)
    saved = volunteer.load_tasks()[task["id"]]
    assert saved["claimed"] == [10, 11]
    assert "full" in interaction.followup.notes[0]
    claim_button = volunteer.task_view(saved).children[0]
    assert claim_button.item.disabled

async def test_unclaim_frees_a_spot():
    task = volunteer.new_task("Setup", "", "", 1, created_by=0)
    task["claimed"] = [10]
    volunteer.save_tasks({task["id"]: task})
    button, interaction = click(task["id"], "unclaim", 10)
    await button.callback(interaction)
    assert volunteer.load_tasks()[task["id"]]["claimed"] == []

async def test_closed_task_refuses():
    task = volunteer.new_task("Old", "", "", 1, created_by=0)
    task["open"] = False
    volunteer.save_tasks({task["id"]: task})
    button, interaction = click(task["id"], "claim", 10)
    sent = []
    async def send_message(text, **kwargs):
        sent.append(text)
    interaction.response.send_message = send_message
    await button.callback(interaction)
    assert sent == ["This task is closed."]
