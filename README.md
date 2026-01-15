# MAX! to MQTT Bridge Add-on Repository

<p align="center">
   <img src="logo.png" width="300" alt="Home Assistant Add-on MAX! to MQTT Bridge">
</p>

This repository contains the MAX! to MQTT Bridge Home Assistant Add-on. 

If you have an existing MAX! installation with e.g. FHEM and you want to migrate to Home Assistant, this might be a solution for you.

I personally want to keep at least my MAX! wall thermostats until there are good alternatives available (battery driven, affordable, modern looking, display...).
But the current solutions to integrate MAX! components into Home Assistant are not ideal:
- The official integration requires the ugly MAX! cube (I threw away this device years ago due to instability)
- The available MAX! add-ons are not really maintained
- The usually recommended usage of Homegear is not a good option: Not maintained anymore and much to overloaded.

Additionally, I was curious to see if I could use AI to help me with the development of this bridge.

This add-on is a lightweight Go-based bridge connecting a CUL-USB stick (868MHz MAX! protocol) to Home Assistant via MQTT. It focuses on usage of an existing MAX! installation with wall thermostats and radiator valves, only supporting the really required functions to be used with Home Assistant. It's not intended as a full replacement of MAX! Cube or the implementation contained in FHEM. 

Read more about this bridge in the [Add-On ReadMe](max2mqtt/README.md) and the [Documentation](max2mqtt/DOCS.md).
