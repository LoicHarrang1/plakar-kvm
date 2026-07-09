# Plugin Plakar KVM / DRBD — Guide d'exploitation

> **Comment importer dans BookStack** : crée un *Livre* « Plakar KVM/DRBD », puis
> une page par chapitre (les titres `##` ci-dessous). Ou colle tout dans une seule
> page via l'éditeur Markdown de BookStack (Édition → Markdown). Le contenu utilise
> du Markdown standard (titres, tableaux, blocs de code, citations).

Ce plugin permet à **Plakar** de sauvegarder et restaurer des machines virtuelles
**KVM/libvirt**, y compris quand leurs disques sont des **volumes DRBD** (block
devices `/dev/drbd…`). Il fonctionne **à distance par SSH** : Plakar n'a pas
besoin d'être installé sur l'hyperviseur.

---

## 1. Ce que fait le plugin

- **Sauvegarde** (connecteur *importer*) : la définition de chaque VM (`domain.xml`)
  + le contenu de ses disques, dans un dépôt Kloset chiffré et dédupliqué.
- **Restauration** (connecteur *exporter*) : redépose les fichiers d'une sauvegarde
  (définition + images disque) sur l'hyperviseur, dans un répertoire dédié.

**Comportement unique, sans réglage :**

| Situation | Comportement |
|-----------|--------------|
| VM **allumée** | Snapshot externe atomique, **crash-consistent** (lecture de la base figée, puis fusion de l'overlay). Aucun agent invité requis. |
| VM **éteinte** | Lecture directe du disque (déjà cohérent). |
| Transport distant | Contrôle libvirt via `qemu+ssh://`, données disque via SSH (compressé). |

> **Note :** « crash-consistent » = équivalent à une coupure de courant. Un système
> de fichiers journalisé (ext4, XFS…) rejoue son journal au démarrage et repart
> proprement. C'est le niveau de cohérence standard pour ce type de sauvegarde.

---

## 2. Architecture

```
+------------------+        SSH (contrôle virsh + données disque)      +-------------------+
|  Hôte Plakar     |  ----------------------------------------------> |   Hyperviseur     |
|  (ex: plakar-srv)|     qemu+ssh:// , ssh cat                         |   KVM + DRBD      |
|  - plakar        |                                                   |   (nœud Primary)  |
|  - plugin kvm    |                                                   |   - VMs à sauver  |
|  - dépôt Kloset  |                                                   +-------------------+
+------------------+
```

Trois rôles :

- **Hôte Plakar** : la machine (souvent une VM dédiée) où tourne Plakar, où est
  installé le plugin, et où réside le dépôt Kloset.
- **Hyperviseur** : le nœud KVM/DRBD qui héberge les VM à sauvegarder.
- **VM invitée** : la VM sauvegardée — **rien à y installer**.

---

## 3. Prérequis par machine

| Composant | Hôte Plakar | Hyperviseur | VM invitée |
|-----------|:-----------:|:-----------:|:----------:|
| `plakar` + plugin `kvm` | ✅ | — | — |
| `virsh` (libvirt-clients) | ✅ | ✅ (déjà présent) | — |
| client / serveur SSH | ✅ client | ✅ serveur | — |
| Accès SSH lecture des disques | — | ✅ (root pour DRBD) | — |

**Aucun `qemu-img` ni `qemu-guest-agent` n'est nécessaire.**

### Hôte Plakar

```bash
apt-get install -y libvirt-clients openssh-client
# + le binaire plakar (selon votre méthode d'installation habituelle)
```

### Hyperviseur

```bash
apt-get install -y openssh-server        # RHEL : dnf install -y openssh-server
```
- libvirt/QEMU sont déjà là (c'est un hyperviseur).
- **DRBD** : les sauvegardes se font sur le nœud **Primary** (seul endroit où
  `/dev/drbd…` est lisible).

### VM invitée

Rien.

---

## 4. Configurer l'accès SSH (obligatoire pour le mode distant)

Depuis l'**hôte Plakar**, un accès SSH **par clé** et **non interactif** vers
l'hyperviseur, avec un utilisateur autorisé à lire les images / block devices
(**`root`** pour les devices DRBD) :

```bash
# sur l'hôte Plakar
test -f ~/.ssh/id_ed25519 || ssh-keygen -t ed25519 -N '' -f ~/.ssh/id_ed25519
ssh-copy-id root@<hyperviseur>

# TEST : doit réussir SANS mot de passe
ssh -o BatchMode=yes root@<hyperviseur> 'virsh version'
```

> **Important :** si ce test demande un mot de passe, le plugin ne fonctionnera
> pas. Vérifiez la clé et les permissions (`~/.ssh` en 700, `authorized_keys` en
> 600 côté hyperviseur).

---

## 5. Construire et installer le plugin

À faire **une fois**, sur une machine disposant de **Go ≥ 1.24** (réseau requis
pour récupérer les dépendances) :

```bash
git clone https://gitea.roullier.net/SysTeam/plakar-kvm.git
cd plakar-kvm
go mod tidy
make build            # produit kvmImporter et kvmExporter
plakar pkg create manifest.yaml v0.3.0
#   -> kvm_v0.3.0_linux_amd64.ptar
```

Installer le paquet sur l'**hôte Plakar** :

```bash
# copier le .ptar sur l'hôte Plakar si besoin, puis :
plakar pkg add ./kvm_v0.3.0_linux_amd64.ptar
plakar pkg show                       # doit lister kvm@v0.3.0 (importer + exporter)
```

> Mise à jour du plugin : rebuild avec un nouveau numéro de version,
> `plakar pkg rm kvm` puis `plakar pkg add` le nouveau `.ptar`.

---

## 6. Sauvegarder

### 6.1 Créer le dépôt Kloset (une fois)

```bash
mkdir -p /var/backups/kvm
plakar at /var/backups/kvm create     # définir une passphrase — À CONSERVER
```

> **La passphrase est irrécupérable si perdue.** Notez-la dans votre coffre de
> secrets.

### 6.2 Déclarer une source et lancer la sauvegarde

La `location` encode tout : `kvm://[<user>@]<hyperviseur>/system`.

```bash
# toutes les VM de l'hyperviseur
plakar source add kvmProd kvm://root@<hyperviseur>/system
plakar at /var/backups/kvm backup @kvmProd

# ou seulement certaines VM
plakar source add kvmWeb kvm://root@<hyperviseur>/system domains=web01,web02
plakar at /var/backups/kvm backup @kvmWeb
```

Pour une **sauvegarde locale** (Plakar installé sur l'hyperviseur) :
`kvm:///system` au lieu de `kvm://root@<hyperviseur>/system`.

### 6.3 Vérifier

```bash
plakar at /var/backups/kvm ls                         # liste des snapshots
plakar at /var/backups/kvm ls <SNAPID>:/              # arborescence
plakar at /var/backups/kvm ls <SNAPID>:/<vm>/disks    # disque(s) + taille
plakar at /var/backups/kvm check <SNAPID>             # intégrité (déchiffre + checksums)
```

Arborescence d'un snapshot :

```
/<vm>/domain.xml
/<vm>/disks/<nom-du-disque>
```

### 6.4 Cas d'une VM Windows

La sauvegarde d'une VM **Windows** se fait **exactement comme une VM Linux** —
aucune commande spécifique, le plugin travaille au niveau **bloc** (contenu des
disques) indépendamment de l'OS invité :

```bash
plakar source add winSrv kvm://root@<hyperviseur>/system domains=<vm-windows>
plakar at /var/backups/kvm backup @winSrv
```

Deux points **spécifiques à Windows** à connaître :

- **Cohérence crash-consistent.** Le disque est capturé comme après une coupure
  de courant. NTFS **rejoue son journal** au démarrage et repart proprement — OK
  pour un serveur de fichiers, un poste, etc. Pour des applications
  transactionnelles (SQL Server, Exchange, Active Directory), la cohérence
  **applicative** exigerait **VSS** (via le qemu-guest-agent Windows / virtio-win)
  — **non pris en charge** par le plugin. Complétez alors par un dump applicatif
  (sauvegarde SQL, etc.).

- **UEFI / NVRAM : capturé ✅.** Les VM Windows (surtout Win10/11) sont quasi
  toujours en **UEFI (OVMF)**, avec un fichier **NVRAM** de variables de démarrage.
  Le plugin le détecte (via `<os><nvram>` dans le `domain.xml`) et le sauvegarde
  sous `/<vm>/nvram/<fichier>` — la VM restaurée **conserve sa config de boot**.
  (Vrai aussi pour une VM Linux en UEFI : c'est le firmware qui compte, pas l'OS.)
- **⚠️ TPM émulé (swtpm) : non capturé.** Si la VM utilise un **TPM** (fréquent
  sur Win11), son **état** n'est pas sauvegardé. Conséquence : si **BitLocker**
  est scellé au TPM, la restauration demandera la **clé de récupération**.

> **Recommandation :** pour un serveur Windows en TPM/BitLocker, conservez à part
> la **clé de récupération BitLocker**. La capture de l'état TPM est prévue (voir
> Limitations).

---

## 7. Restaurer

La restauration **dépose les fichiers** de la sauvegarde sur l'hyperviseur (par
SSH), dans un répertoire dédié. Elle **ne redéfinit pas** et **ne démarre pas** la
VM, et **ne touche jamais** aux devices DRBD de production — la remise en service
est un geste admin explicite.

```bash
plakar at /var/backups/kvm restore -to kvm://root@<hyperviseur>/system <SNAPID>
```

Résultat sur l'hyperviseur :

```
/var/lib/libvirt/images/plakar-restore/<vm>/domain.xml
/var/lib/libvirt/images/plakar-restore/<vm>/disks/<nom-du-disque>
```

> **Espace :** restaurer un disque de N Go écrit un fichier de N Go. Vérifiez
> `df -h /var/lib/libvirt/images` sur l'hyperviseur avant.

### Remettre la VM en service (manuel)

Sur l'hyperviseur, deux options :

**Option A — démarrer depuis le fichier restauré** (test / DR rapide) :
```bash
cd /var/lib/libvirt/images/plakar-restore/<vm>
# éditer domain.xml :
#  - renommer la VM, retirer <uuid> et <mac> pour éviter les conflits
#  - pointer <disk><source> vers disks/<nom>
#  - VM UEFI : pointer <nvram> vers nvram/<fichier> (ou recopier ce fichier
#    dans /var/lib/libvirt/qemu/nvram/) pour conserver la config de boot
virsh define domain.xml
virsh start <vm>
```

**Option B — réécrire le disque sur un volume dédié** :
```bash
qemu-img convert -O raw disks/<nom> /dev/<volume-de-test>
# puis define avec le XML pointant sur ce volume
```

> **DRBD :** pour un test, écrivez sur un volume **dédié/neuf** — n'écrasez
> jamais le device Primary de production sans fenêtre de maintenance.

---

## 8. Limitations connues

- **Pas de sauvegarde incrémentale** sur disques **raw** (dont DRBD) : chaque
  sauvegarde **relit le disque entier**. C'est une limite de libvirt/QEMU (les
  dirty bitmaps persistants exigent du qcow2). **Conséquence :** le *temps de
  lecture* est proportionnel à la taille provisionnée du disque, mais le
  *stockage* reste petit grâce à la déduplication Kloset (les zéros et les données
  inchangées ne sont pas re-stockés).
  - Conseil : planifier les sauvegardes **hors heures de production**.
- **Cohérence crash-consistent** uniquement (pas de quiesce applicatif). Adapté
  aux FS journalisés et à la plupart des charges. Les bases de données très
  sensibles peuvent nécessiter un dump applicatif complémentaire.
- **État TPM (swtpm) non capturé** (impacte surtout Win11 avec TPM) : le
  `domain.xml`, le **NVRAM UEFI** et les disques sont sauvegardés, mais **pas**
  l'état du TPM émulé. Une VM avec **BitLocker** scellé au TPM demandera sa **clé
  de récupération** à la restauration. *(Capture de l'état TPM prévue — roadmap.)*
- **Restauration semi-automatique** : les fichiers sont restaurés, la
  redéfinition de la VM est manuelle (par sécurité).

---

## 9. Sécurité / durcissement production

- **Utilisateur SSH dédié** plutôt que `root` : sur l'hyperviseur,
  `useradd -m -G libvirt,disk backup` (groupe `libvirt` pour piloter les VM,
  `disk` pour lire les block devices), puis `location = kvm://backup@<hv>/system`.
- **Clé SSH dédiée et restreinte** (dans `authorized_keys` de l'hyperviseur) :
  `from="<ip-hôte-plakar>",no-port-forwarding,no-pty ssh-ed25519 AAAA...`.
- **Passphrase du dépôt** : via un gestionnaire de secrets, pas en clair.
- **Permissions** : clé privée et config Plakar en `600`, répertoire `700`.
- **Réseau** : SSH sur un réseau d'administration filtré.
- L'**hôte Plakar** peut lire toutes les VM → traitez-le comme un **actif
  sensible** (Tier-0), isolé et durci.

---

## 10. Dépannage

| Symptôme | Cause probable | Action |
|----------|----------------|--------|
| SSH demande un mot de passe | clé absente / mauvaises permissions | refaire `ssh-copy-id`, vérifier `~/.ssh` (700), `authorized_keys` (600) |
| `virsh: command not found` (plugin) | `libvirt-clients` manquant sur l'hôte Plakar | `apt-get install libvirt-clients` |
| Snapshot vide (0 B) au parcours | filtre `domains=` qui ne matche aucune VM | vérifier le nom exact : `virsh -c qemu+ssh://root@<hv>/system list --all --name` |
| Backup « bloqué » vers X Mio | disque volumineux lu en entier (les zéros ne font pas monter le compteur) | patienter ; vérifier que la lecture avance (`grep drbd /proc/diskstats` sur l'hyperviseur) |
| `snapshot-create-as` échoue | overlay orphelin d'un backup interrompu | `virsh blockcommit <vm> <cible> --active --pivot --wait` puis supprimer l'overlay `…-plakar-*.qcow2` |
| Overlay `…-plakar-*.qcow2` résiduel après backup | backup interrompu (ne jamais couper un backup) | idem : `blockcommit --active --pivot --wait` + `rm` de l'overlay |
| Restauration : plus d'espace | fichier disque volumineux | libérer / cibler un autre volume ; `df -h /var/lib/libvirt/images` |

> **Ne jamais interrompre un backup en cours** : cela laisse un overlay attaché à
> la VM. Si c'est arrivé, récupérez avec `blockcommit --active --pivot --wait`.

---

## 11. Référence rapide

**Options de configuration** (source *et* destination) :

| Clé | Obligatoire | Description |
|-----|:-----------:|-------------|
| `location` | ✅ | `kvm://[<user>@]<hyperviseur>/system` (ou `kvm:///system` en local) |
| `domains` | — | liste de VM séparées par des virgules (défaut : toutes) — *sauvegarde uniquement* |

**Commandes essentielles :**

```bash
# dépôt
plakar at <repo> create
plakar at <repo> ls
plakar at <repo> check <SNAPID>

# source / sauvegarde
plakar source add <nom> kvm://root@<hv>/system [domains=vm1,vm2]
plakar at <repo> backup @<nom>

# restauration
plakar at <repo> restore -to kvm://root@<hv>/system <SNAPID>

# plugin
plakar pkg show
plakar pkg add ./kvm_vX.Y.Z_linux_amd64.ptar
```

---

## 12. Poste Windows

> **À savoir :** le plugin s'exécute sur l'**hôte Plakar** et a besoin de `virsh`
> (client libvirt), qui **n'existe pas nativement sous Windows**. La voie propre
> sous Windows est donc **WSL2**. Windows natif sert au **build** et à la gestion
> des **clés SSH** / **git**, pas à exécuter le plugin.

### 12.1 Voie recommandée — WSL2 (Debian/Ubuntu)

Dans WSL2 on retrouve tout le toolchain Linux (`virsh`, `ssh`, `go`, `plakar`),
donc **toutes les instructions Linux de ce guide (chapitres 3 à 7) s'appliquent
telles quelles** à l'intérieur de WSL.

```powershell
# PowerShell (administrateur) — installer WSL2 + Debian
wsl --install -d Debian
```
Puis, dans le terminal WSL :
```bash
sudo apt-get update
sudo apt-get install -y libvirt-clients openssh-client
# + installer plakar dans WSL, puis suivre les chapitres 4 à 7
```

### 12.2 Windows natif — clés SSH (client OpenSSH intégré)

Windows 10/11 fournit `ssh` et `ssh-keygen`, mais **pas** `ssh-copy-id` : on copie
la clé à la main.

```powershell
# générer une clé (si besoin)
ssh-keygen -t ed25519

# copier la clé publique vers l'hyperviseur
type $env:USERPROFILE\.ssh\id_ed25519.pub | ssh root@<hyperviseur> "mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys"

# test : doit réussir SANS mot de passe
ssh -o BatchMode=yes root@<hyperviseur> "virsh version"
```

### 12.3 Windows natif — compiler le plugin (cross-compilation vers Linux)

Les binaires du plugin ciblent **Linux** (ils tournent sur l'hôte Plakar). Depuis
Windows on cross-compile :

```powershell
# PowerShell — dans le dossier du dépôt
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -o kvmImporter ./plugin/importer
go build -o kvmExporter ./plugin/exporter
```

> Le **packaging** (`plakar pkg create`) et l'**installation** (`plakar pkg add`)
> se font sur l'**hôte Plakar Linux** (ou dans WSL2), là où le plugin s'exécute.
> Windows natif reste un poste de **build / git / PR**.

### 12.4 Windows natif — git / contribution

Git for Windows (Git Bash ou PowerShell) fonctionne normalement pour cloner,
committer et pousser vers le dépôt (voir la procédure de contribution / PR).

> Résumé : **exécuter le plugin → Linux ou WSL2** ; **builder / gérer les clés /
> git → Windows natif possible**.

---

*Dépôt du plugin : `github.com/LoicHarrang1/plakar-kvm`. Contact :
loic.harrang@roullier.com.*
