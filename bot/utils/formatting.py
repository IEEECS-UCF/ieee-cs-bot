import discord
import datetime

IEEE_BLUE = 0x00629B
IEEE_GOLD = 0xF2A900
ERROR_RED = 0xE74C3C
SUCCESS_GREEN = 0x2ECC71

def create_base_embed(title: str, description: str, color: int = IEEE_BLUE) -> discord.Embed:
    embed = discord.Embed(
        title=title,
        description=description,
        color=color,
        timestamp=discord.utils.utcnow()
    )
    embed.set_footer(text="IEEE CS at UCF")
    return embed

def format_timestamp(dt: datetime.datetime, style: str = "F") -> str:
    unix_time = int(dt.timestamp())
    return f"<t:{unix_time}:{style}>"

def truncate_text(text: str, max_length: int = 1024) -> str:
    if len(text) > max_length:
        return text[:max_length - 3] + "..."
    return text
