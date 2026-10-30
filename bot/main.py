import asyncio
import logging
import os
import discord
from discord.ext import commands
from dotenv import load_dotenv

from bot.utils.alerts import alert

load_dotenv()

class IEEECSBot(commands.Bot):
    def __init__(self):
        intents = discord.Intents.default()
        intents.message_content = True
        intents.guild_scheduled_events = True
        # Privileged: also switch on "Server Members Intent" in the Developer Portal first
        intents.members = os.getenv('MEMBERS_INTENT') == '1'  
        
        super().__init__(
            command_prefix=commands.when_mentioned_or('!'),
            intents=intents,
            help_command=commands.DefaultHelpCommand()
        )

    async def setup_hook(self):
        """Automatically load all cogs and sync slash commands on startup."""
        cogs_dir = './bot/cogs'
        
        if not os.path.exists(cogs_dir):
            os.makedirs(cogs_dir)

        for filename in os.listdir(cogs_dir):
            if filename.endswith('.py') and not filename.startswith('__'):
                extension_name = f'bot.cogs.{filename[:-3]}'
                try:
                    await self.load_extension(extension_name)
                    print(f"Loaded extension: {extension_name}")
                except Exception as e:
                    print(f"Failed to load extension {extension_name}: {e}")
        
        self.tree.on_error = self.on_app_command_error
        await self.tree.sync()
        print("Slash commands synced.")

    async def on_app_command_error(self, interaction, error):
        """Log slash-command failures and tell the user instead of failing silently."""
        if isinstance(error, discord.app_commands.CheckFailure):
            return  # the check already told the user why
        name = interaction.command.qualified_name if interaction.command else "?"
        await alert(self, f"Command /{name}", getattr(error, 'original', error))
        message = "Something went wrong running that command. It has been logged."
        try:
            if interaction.response.is_done():
                await interaction.followup.send(message, ephemeral=True)
            else:
                await interaction.response.send_message(message, ephemeral=True)
        except discord.HTTPException:
            pass

    async def on_ready(self):
        print(f'Logged in as {self.user} (ID: {self.user.id})')
        print('Ready to serve IEEE CS at UCF.')
        print('-----------------------------------')

async def main():
    token = os.getenv('DISCORD_TOKEN')
    if not token:
        raise ValueError("DISCORD_TOKEN not found. Please check your .env file.")
    
    discord.utils.setup_logging(level=logging.INFO)
    bot = IEEECSBot()
    
    async with bot:
        await bot.start(token)

if __name__ == '__main__':
    asyncio.run(main())
