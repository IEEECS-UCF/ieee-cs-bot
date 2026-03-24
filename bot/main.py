import asyncio
import os
import discord
from discord.ext import commands
from dotenv import load_dotenv

load_dotenv()

class IEEECSBot(commands.Bot):
    def __init__(self):
        intents = discord.Intents.default()
        intents.message_content = True
        intents.guild_scheduled_events = True  
        
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
        
        await self.tree.sync()
        print("Slash commands synced.")

    async def on_ready(self):
        print(f'Logged in as {self.user} (ID: {self.user.id})')
        print('Ready to serve IEEE CS at UCF.')
        print('-----------------------------------')

async def main():
    token = os.getenv('DISCORD_TOKEN')
    if not token:
        raise ValueError("DISCORD_TOKEN not found. Please check your .env file.")
    
    bot = IEEECSBot()
    
    async with bot:
        await bot.start(token)

if __name__ == '__main__':
    asyncio.run(main())
