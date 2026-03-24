import os
import json
import datetime
import discord
from discord.ext import commands, tasks
from dateutil import parser
from bs4 import BeautifulSoup
from bot.utils.gcal_client import fetch_upcoming_events
from bot.utils.formatting import create_base_embed, format_timestamp, IEEE_GOLD

STATE_FILE = "data/state.json"

class CalendarSync(commands.Cog):
    def __init__(self, bot):
        self.bot = bot
        self.calendar_id = os.getenv('GOOGLE_CALENDAR_ID')
        self.guild_id = int(os.getenv('GUILD_ID'))
        self.events_channel_id = int(os.getenv('EVENTS_CHANNEL_ID'))
        
        os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
        self.sync_events.start()

    def cog_unload(self):
        self.sync_events.cancel()

    def get_saved_message_id(self):
        if os.path.exists(STATE_FILE):
            with open(STATE_FILE, "r") as f:
                data = json.load(f)
                return data.get("summary_message_id")
        return None

    def save_message_id(self, message_id):
        with open(STATE_FILE, "w") as f:
            json.dump({"summary_message_id": message_id}, f)

    @tasks.loop(minutes=15)
    async def sync_events(self):
        await self.bot.wait_until_ready()
        guild = self.bot.get_guild(self.guild_id)
        channel = guild.get_channel(self.events_channel_id) if guild else None
        
        if not guild or not channel:
            print("Error: Could not find guild or events channel.")
            return

        print("Syncing Google Calendar events to Discord...")
        
        try:
            gcal_events = fetch_upcoming_events(self.calendar_id, days=14)
        except Exception as e:
            print(f"Failed to fetch calendar events: {e}")
            return

        existing_events = await guild.fetch_scheduled_events()
        
        for item in gcal_events:
            name = item.get('summary', 'Untitled Event')
            
            start_str = item['start'].get('dateTime', item['start'].get('date'))
            end_str = item['end'].get('dateTime', item['end'].get('date'))
            start_time = parser.isoparse(start_str)
            end_time = parser.isoparse(end_str)
            
            raw_description = item.get('description', 'No description provided.')
            description = BeautifulSoup(raw_description, "html.parser").get_text(separator='\n\n')
            location = item.get('location', 'TBA')

            existing_event = discord.utils.get(existing_events, name=name)

            if existing_event:
                needs_update = (
                    existing_event.description != description or
                    existing_event.location != location or
                    existing_event.start_time != start_time or
                    existing_event.end_time != end_time
                )
                if needs_update:
                    try:
                        await existing_event.edit(
                            description=description, location=location,
                            start_time=start_time, end_time=end_time
                        )
                        print(f"Updated existing Discord event: {name}")
                    except Exception as e:
                        print(f"Failed to update event '{name}': {e}")
            else:
                try:
                    await guild.create_scheduled_event(
                        name=name, description=description,
                        start_time=start_time, end_time=end_time,
                        entity_type=discord.EntityType.external,
                        location=location, privacy_level=discord.PrivacyLevel.guild_only
                    )
                    print(f"Created new Discord event: {name}")
                except Exception as e:
                    print(f"Failed to create event '{name}': {e}")

        embed = create_base_embed(
            title="📅 Upcoming IEEE CS Events (Next 7 Days)",
            description="Here is what we have planned for the upcoming week!",
            color=IEEE_GOLD
        )
        embed.set_footer(text="Last synced | IEEE CS at UCF")

        now = discord.utils.utcnow()
        next_week = now + datetime.timedelta(days=7)
        weekly_events = []

        for item in gcal_events:
            start_str = item['start'].get('dateTime', item['start'].get('date'))
            start_time = parser.isoparse(start_str)
            
            if start_time.tzinfo is None:
                start_time = start_time.replace(tzinfo=datetime.timezone.utc)

            if now <= start_time <= next_week:
                weekly_events.append((item, start_time))

        if not weekly_events:
            embed.add_field(name="No events scheduled.", value="Check back later!", inline=False)
        else:
            for item, start_time in weekly_events[:25]:
                name = item.get('summary', 'Untitled Event')
                location = item.get('location', 'TBA')
                
                time_str = format_timestamp(start_time, style="F")
                relative_time_str = format_timestamp(start_time, style="R")
                
                embed.add_field(
                    name=name,
                    value=f"**Time:** {time_str} ({relative_time_str})\n**Location:** {location}",
                    inline=False
                )

        saved_msg_id = self.get_saved_message_id()
        summary_message = None

        if saved_msg_id:
            try:
                summary_message = await channel.fetch_message(saved_msg_id)
            except discord.NotFound:
                pass 

        if summary_message:
            await summary_message.edit(embed=embed)
            print("Updated the weekly summary message.")
        else:
            summary_message = await channel.send(embed=embed)
            self.save_message_id(summary_message.id)
            print("Created a new weekly summary message.")

    @sync_events.before_loop
    async def before_sync(self):
        await self.bot.wait_until_ready()

async def setup(bot):
    await bot.add_cog(CalendarSync(bot))
