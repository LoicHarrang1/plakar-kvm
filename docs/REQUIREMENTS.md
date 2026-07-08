# Prérequis de déploiement — intégration KVM/DRBD (mode distant)

Trois rôles. Les commandes `apt` sont pour Debian/Ubuntu (équivalents RHEL entre
parenthèses).

Matrice « qui a besoin de quoi » :

| Composant | Hôte Plakar | Hyperviseur | VM invitée |
|-----------|:-----------:|:-----------:|:----------:|
| plakar + plugin kvm | ✅ | — | — |
| `virsh` (client libvirt) | ✅ | ✅ (déjà) | — |
| `ssh` client / serveur | ✅ (client) | ✅ (serveur) | — |
| `qemu-img` | — | ⚠️ (fsfreeze only) | — |
| libvirt/qemu (hyperviseur) | — | ✅ (déjà) | — |
| qemu-guest-agent | — | — | ⚠️ (quiesce only) |

---

## A. Hôte Plakar (ex. `plakar-server`) — d'où on lance les backups

**Paquets :**
```bash
apt-get install -y libvirt-clients openssh-client
# + le binaire plakar (selon ta méthode d'installation)
```
- `libvirt-clients` → fournit `virsh` (le plugin lance `virsh -c qemu+ssh://…`).
- `openssh-client` → fournit `ssh` (lecture des disques : `ssh cat`, `qemu-img` distant).

**Plugin :**
```bash
plakar pkg add ./kvm_v0.1.0_linux_amd64.ptar
plakar pkg show          # doit lister kvm@v0.1.0
```

**Accès SSH non interactif vers l'hyperviseur :**
```bash
test -f ~/.ssh/id_ed25519 || ssh-keygen -t ed25519 -N '' -f ~/.ssh/id_ed25519
ssh-copy-id root@FR-KVM-TEST1
# TEST décisif : doit passer SANS mot de passe
ssh -o BatchMode=yes root@FR-KVM-TEST1 'virsh version && qemu-img --version'
```

**Dépôt Kloset :** (local ici)
```bash
mkdir -p /var/backups/kvm && plakar at /var/backups/kvm create
```

---

## B. Hyperviseur (ex. `FR-KVM-TEST1`) — où sont les VM/disques

C'est un hôte KVM, donc libvirt/qemu sont déjà là. À ajouter/vérifier :

**Paquets :**
```bash
apt-get install -y openssh-server qemu-utils
# RHEL : dnf install -y openssh-server qemu-img
```
- `openssh-server` → accès SSH depuis l'hôte Plakar.
- `qemu-utils` (fournit `qemu-img`) → **requis uniquement pour le mode `fsfreeze`**
  (conversion des disques). **Les modes `crash` et `snapshot` n'en ont pas besoin.**
  (C'était le `qemu-img: command not found` du début.)

**Déjà présents (à vérifier) :** `libvirtd` actif, et les utilitaires standard
`cat`, `stat`, `blockdev`, `mktemp`, `rm` (coreutils/util-linux).

**Configuration :**
- Le compte SSH utilisé (souvent **`root`**) doit pouvoir **lire les images /
  block devices** — indispensable pour les devices DRBD `/dev/drbdN`.
- **DRBD** : cibler le nœud **Primary** (seul endroit où `/dev/drbdN` est lisible).
- Mode `snapshot` : le répertoire `overlay_dir` (défaut `/var/lib/libvirt/images`)
  doit être **inscriptible par le process QEMU** (c'est le cas par défaut).

---

## C. VM à sauvegarder (invité, ex. `plakar-backup-repo`)

**Pour les modes `crash` et `snapshot` : RIEN à installer dans l'invité.**
Tout se passe au niveau hyperviseur. C'est le cas d'usage recommandé.

**Uniquement pour la cohérence applicative** (`snapshot --quiesce` ou `fsfreeze`) :
```bash
apt-get install -y qemu-guest-agent
systemctl start qemu-guest-agent
# RHEL : dnf install -y qemu-guest-agent && systemctl start qemu-guest-agent
```
+ côté hyperviseur, le canal virtio doit exister dans le XML de la VM
(`virsh edit <dom>`) :
```xml
<channel type='unix'>
  <target type='virtio' name='org.qemu.guest_agent.0'/>
</channel>
```
+ les RPC `guest-fsfreeze-*` doivent être autorisées côté agent.

> ⚠️ **Attention** : un `fsfreeze` sans `thaw` **gèle la VM** (elle ne répond
> plus). Récupération : `virsh domfsthaw <dom>` depuis l'hyperviseur. Le mode
> `snapshot` gère freeze+thaw atomiquement et retombe tout seul en
> crash-consistent si l'agent n'est pas dispo → **préféré**.
>
> ⚠️ Ne **jamais** quiescer la VM qui héberge Plakar elle-même.

---

## Résumé minimal pour ton setup validé (distant + snapshot + raw/DRBD)

- **Hôte Plakar** : `libvirt-clients`, `openssh-client`, plakar + plugin, clé SSH.
- **Hyperviseur** : `openssh-server`, accès SSH root, être sur le Primary DRBD.
  (`qemu-utils` seulement si tu veux aussi le mode `fsfreeze`.)
- **VM invitée** : rien.

## Build du plugin (à faire une fois, n'importe où)

Go ≥ 1.24 :
```bash
cd integration-kvm
go mod tidy && make build
plakar pkg create manifest.yaml v0.1.0   # -> kvm_v0.1.0_linux_amd64.ptar
```
