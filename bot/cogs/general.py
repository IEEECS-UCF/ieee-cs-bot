import discord
from discord.ext import commands
from discord import app_commands
from bot.utils.formatting import create_base_embed, IEEE_GOLD, IEEE_BLUE

class General(commands.Cog):
    def __init__(self, bot):
        self.bot = bot

    @app_commands.command(name="socials", description="Get the links to IEEE CS UCF social media and platforms.")
    async def socials(self, interaction: discord.Interaction):
        """Replies with an embed containing social media links."""
        embed = create_base_embed(
            title="📱 IEEE CS UCF Socials", 
            description="Connect with us on our platforms!", 
            color=IEEE_BLUE
        )
        embed.add_field(name="Website", value="[tbd]()", inline=False)
        embed.add_field(name="Instagram", value="[@ieeecsucf](https://instagram.com)", inline=False)
        embed.add_field(name="Knight Connect", value="[Join Here](https://knightconnect.campuslabs.com/)", inline=False)
        
        await interaction.response.send_message(embed=embed)

    @app_commands.command(name="info", description="Learn more about the IEEE Computer Society at UCF.")
    async def info(self, interaction: discord.Interaction):
        """Replies with general information about the club based on official branding."""
        
        about_text = (
            "Hi! Welcome to the IEEE Computer Society at UCF! We are a technical "
            "organization with a focus on low level systems, computer architecture, and computer science theory!"
        )
        
        embed = create_base_embed(
            title="About IEEE CS at UCF", 
            description=about_text, 
            color=IEEE_GOLD
        )
        
        embed.add_field(
            name="Fields of Interest", 
            value="Systems Programming, Comp Arch, GPU Programming, Embedded Systems, OS, Drivers, Computation, Discrete Mathematics.", 
            inline=False
        )
        
        await interaction.response.send_message(embed=embed)
        
async def setup(bot):
    await bot.add_cog(General(bot))
