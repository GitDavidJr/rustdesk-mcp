# Raio-x da máquina para integração: só leitura, sem abrir arquivo de dados nem ler senha.
$ErrorActionPreference='SilentlyContinue'
function Titulo($t){ "`n== $t" }
Titulo 'Sistema'
$os=Get-CimInstance Win32_OperatingSystem; $cs=Get-CimInstance Win32_ComputerSystem
"$($os.Caption) $($os.Version) $($os.OSArchitecture) | máquina $($cs.Name) | domínio $($cs.Domain) | RAM $([math]::Round($cs.TotalPhysicalMemory/1GB)) GB | ligada desde $($os.LastBootUpTime)"
$id=[Security.Principal.WindowsIdentity]::GetCurrent(); $adm=([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
"usuário $($id.Name) | terminal com administrador: $adm | usuário no grupo de administradores: $([bool](whoami /groups | Select-String 'S-1-5-32-544'))"
Titulo 'Discos e unidades de rede'
Get-CimInstance Win32_LogicalDisk | ForEach-Object { "$($_.DeviceID) tipo $($_.DriveType) $($_.FileSystem) livre $([math]::Round($_.FreeSpace/1GB)) de $([math]::Round($_.Size/1GB)) GB $($_.ProviderName)" }
Titulo 'Serviços de banco, web, túnel e ERP'
$padrao='sql|firebird|interbase|oracle|mysql|maria|postgres|mongo|sybase|anywhere|progress|informix|db2|pervasive|btrieve|dbisam|advantage|hasp|sentinel|tomcat|apache|nginx|iis|w3svc|jboss|wildfly|cloudflared|rustdesk|anydesk|teamviewer|erp|nfe|sat|tef|pdv'
Get-CimInstance Win32_Service | Where-Object { ($_.Name+' '+$_.DisplayName+' '+$_.PathName) -match $padrao } | Select-Object -First 60 | ForEach-Object { "$($_.State) | $($_.StartMode) | $($_.Name) | $($_.StartName) | $($_.PathName)" }
Titulo 'Portas em escuta'
Get-NetTCPConnection -State Listen | Group-Object LocalPort | ForEach-Object { $c=$_.Group[0]; $p=Get-Process -Id $c.OwningProcess; [pscustomobject]@{Porta=[int]$c.LocalPort;Ende=($_.Group.LocalAddress|Select-Object -Unique) -join ',';Processo=$p.ProcessName;Caminho=$p.Path} } | Where-Object { $_.Porta -lt 49152 -or $_.Processo -notmatch '^(svchost|lsass|services|wininit|spoolsv)$' } | Sort-Object Porta | ForEach-Object { "$($_.Porta) | $($_.Ende) | $($_.Processo) | $($_.Caminho)" }
Titulo 'Conexões para banco (portas comuns)'
Get-NetTCPConnection -State Established | Where-Object { $_.RemotePort -in 1433,1434,3050,3051,1521,3306,3307,5432,5433,27017,1972,2638,5000,8080 } | Select-Object -First 40 | ForEach-Object { $p=Get-Process -Id $_.OwningProcess; "$($p.ProcessName) -> $($_.RemoteAddress):$($_.RemotePort) | $($p.Path)" }
Titulo 'Janelas abertas'
Get-Process | Where-Object MainWindowTitle | ForEach-Object { "$($_.ProcessName) | $($_.MainWindowTitle) | $($_.Path)" }
Titulo 'Atalhos da área de trabalho'
$sh=New-Object -ComObject WScript.Shell
Get-ChildItem "$env:PUBLIC\Desktop\*.lnk","$env:USERPROFILE\Desktop\*.lnk" | ForEach-Object { $l=$sh.CreateShortcut($_.FullName); "$($_.BaseName) | $($l.TargetPath) $($l.Arguments) | em $($l.WorkingDirectory)" }
Titulo 'Programas instalados'
Get-ItemProperty HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*,HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*,HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\* | Where-Object { $_.DisplayName -and $_.DisplayName -notmatch 'Update|Redistributable|Microsoft (Edge|OneDrive)|Language Pack' } | Sort-Object DisplayName -Unique | Select-Object -First 120 | ForEach-Object { "$($_.DisplayName) | $($_.DisplayVersion) | $($_.Publisher) | $($_.InstallLocation)" }
Titulo 'Fontes ODBC'
Get-OdbcDsn | ForEach-Object { "$($_.Name) | $($_.DriverName) | $($_.Platform) | $($_.DsnType)" }
Get-OdbcDriver | Select-Object -ExpandProperty Name -Unique | ForEach-Object { "driver: $_" }
Titulo 'Instâncias de SQL Server'
(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Microsoft SQL Server').InstalledInstances
Titulo 'Arquivos de banco e de configuração (só o nome) em pastas de programa'
Get-ChildItem C:\ -Directory | Where-Object { $_.Name -notmatch '^(Windows|Users|PerfLogs|\$|Recovery|ProgramData|System Volume)' } | ForEach-Object { Get-ChildItem $_.FullName -Recurse -Depth 3 -File -Include *.fdb,*.gdb,*.mdf,*.mdb,*.accdb,*.dbf,*.db,*.sqlite,*.udl,*.ini,*.config,*.conf,tnsnames.ora,aliases.conf,databases.conf | Where-Object { $_.FullName -notmatch '\\(Windows Kits|Microsoft|WindowsApps|Common Files|dotnet|Reference Assemblies)\\' } } | Sort-Object LastWriteTime -Descending | Select-Object -First 80 | ForEach-Object { "$($_.LastWriteTime.ToString('yyyy-MM-dd HH:mm')) | $([math]::Round($_.Length/1MB,1)) MB | $($_.FullName)" }
Titulo 'Tarefas agendadas fora do Windows'
Get-ScheduledTask | Where-Object { $_.TaskPath -notmatch '^\\Microsoft' } | Select-Object -First 30 | ForEach-Object { "$($_.State) | $($_.TaskName) | $(($_.Actions | ForEach-Object { $_.Execute+' '+$_.Arguments }) -join '; ')" }
Titulo 'Rede'
Get-NetIPConfiguration | Where-Object IPv4Address | ForEach-Object { "$($_.InterfaceAlias) | $($_.IPv4Address.IPAddress) | gw $($_.IPv4DefaultGateway.NextHop) | dns $(($_.DNSServer.ServerAddresses) -join ',')" }
(Get-Content C:\Windows\System32\drivers\etc\hosts) | Where-Object { $_ -match '^\s*[0-9]' }
